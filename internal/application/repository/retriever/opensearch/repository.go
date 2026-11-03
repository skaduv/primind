package opensearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/opensearch-project/opensearch-go/v4"
	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

// Repository shares an immutable client/config and per-index initialization results.
// Each attempt publishes its own error before waking waiters. Transient failures
// are removed only for future callers, so existing waiters cannot see false success.
type Repository struct {
	client    *osapi.Client
	baseIndex string      // pre-suffix base name, e.g. "primind_abcdef012345"
	cfg       internalCfg // immutable after construction (value, not pointer)

	initMu          sync.Mutex
	initializations map[int]*indexInitialization
	keywordsMu      sync.Mutex
	keywordsReady   bool

	// sink receives audit events (index created / reindex executed). nil
	// means no auditing; use r.auditSink() to get a non-nil sink. Set via
	// WithAuditSink at construction.
	sink          AuditSink
	writeObserver func(WriteObservation)
}

// Compile-time interface satisfaction (Go best practice — keeps the build
// red if the interface drifts and our implementation lags).
var _ interfaces.RetrieveEngineRepository = (*Repository)(nil)

// NewRepository builds a new OpenSearch k-NN repository and verifies the
// backing cluster is reachable + version-compatible + has the k-NN
// plugin installed on every cluster node. It does NOT create any index —
// callers (Save / Retrieve) trigger lazy per-dimension creation via
// ensureReady on first use.
//
// storeID is the VectorStore.ID owning this repository instance. It is
// folded into the base index name so multiple OpenSearch VectorStores
// pointing at the same cluster do not collide:
//   - storeID == ""       — env-path; no prefix, base index = "primind".
//   - len(storeID) >= 16  — DB-store path; prefix = storeID[:12]
//     (48-bit collision space).
//   - 1..15 chars         — rejected with ErrConfigInvalid.
//
// indexCfg is optional — pass nil to use env var (OPENSEARCH_INDEX) or
// default ("primind") values.
//
// Optional behavior is configured via functional options (e.g.
// WithAuditSink). Passing no options keeps audit emission as a no-op, so the
// env-path and tests need no extra wiring.
//
// Returns a typed sentinel error wrapped with %w; callers translate to
// AppError at the engine-factory boundary.
func NewRepository(
	ctx context.Context,
	client *osapi.Client,
	storeID string,
	indexCfg *types.IndexConfig,
	opts ...Option,
) (interfaces.RetrieveEngineRepository, error) {
	ctx, cancel := operationContext(ctx, client)
	defer cancel()
	log := logger.GetLogger(ctx)

	if storeID != "" && len(storeID) < 16 {
		return nil, fmt.Errorf(
			"opensearch: storeID must be empty or >=16 chars, got %d: %w",
			len(storeID), ErrConfigInvalid,
		)
	}

	base := types.ResolveIndexName(indexCfg, "OPENSEARCH_INDEX", "primind")
	if storeID != "" {
		// 12 hex chars → 48-bit collision space (UUID is 32 chars; using
		// half gives strong-enough collision avoidance for the realistic
		// "two OpenSearch VectorStores on the same cluster" case).
		base = fmt.Sprintf("%s_%s", base, storeID[:12])
	}
	base, err := sanitizeIndexName(base)
	if err != nil {
		return nil, fmt.Errorf("opensearch: invalid index base name: %w", err)
	}

	icfg, err := buildInternalCfg(indexCfg)
	if err != nil {
		return nil, fmt.Errorf("opensearch: invalid index config: %w", err)
	}

	if err := probeVersion(ctx, client); err != nil {
		return nil, err // already wraps ErrVersionUnsupported / ErrTransport
	}
	if err := probeKNNPlugin(ctx, client); err != nil {
		return nil, err // already wraps ErrConfigInvalid / ErrTransport
	}

	r := &Repository{
		client:          client,
		baseIndex:       base,
		cfg:             icfg,
		initializations: make(map[int]*indexInitialization),
	}
	for _, opt := range opts {
		opt(r)
	}
	log.Infof("[OpenSearch] repository ready (baseIndex=%s, knn_engine=%s, hnsw_m=%d)",
		base, icfg.knnEngine, icfg.hnswM)
	return r, nil
}

// indexInitialization is immutable after done is closed.
type indexInitialization struct {
	done chan struct{}
	err  error
}

func (r *Repository) ensureReady(ctx context.Context, dim int) error {
	if ctx == nil {
		return fmt.Errorf("opensearch: ensureReady requires non-nil ctx")
	}
	if dim <= 0 || dim > 16000 {
		return fmt.Errorf("opensearch: dim %d out of range (1..16000): %w", dim, ErrDimensionMismatch)
	}
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	return r.ensureIndex(ctx, dim, func(ownedCtx context.Context) error { return r.createIndexAndAlias(ownedCtx, dim) })
}

func waitForInitialization(ctx context.Context, attempt *indexInitialization) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-attempt.done:
		return attempt.err
	}
}

func (r *Repository) ensureIndex(ctx context.Context, dim int, create func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.initMu.Lock()
	existing := r.initializations[dim]
	r.initMu.Unlock()
	if existing != nil {
		return waitForInitialization(ctx, existing)
	}

	// Acquire outside initMu, before publishing: otherwise admitted BatchSave
	// waiters could consume every write slot needed by this initializer's PUT.
	// A BatchSave initializer borrows its already-held encoding/write lease.
	ctx, release, err := admitWriteOperation(ctx, r.client)
	if err != nil {
		return err
	}
	r.initMu.Lock()
	if existing = r.initializations[dim]; existing != nil {
		r.initMu.Unlock()
		release() // another owner won while admission was pending
		return waitForInitialization(ctx, existing)
	}
	if r.initializations == nil {
		r.initializations = make(map[int]*indexInitialization)
	}
	attempt := &indexInitialization{done: make(chan struct{})}
	r.initializations[dim] = attempt
	r.initMu.Unlock()
	defer release()
	attempt.err = create(ctx)
	r.initMu.Lock()
	close(attempt.done)
	if isTransientErr(attempt.err) || errors.Is(attempt.err, context.Canceled) || errors.Is(attempt.err, context.DeadlineExceeded) {
		delete(r.initializations, dim)
	}
	r.initMu.Unlock()
	return attempt.err
}

// indexAlias returns the alias name for a given dimension. All reads /
// writes target the alias; the alias points at the actual <alias>_v1
// index, enabling future rolling-reindex swap workflows (the swap path
// itself is deferred to a follow-up PR; see stubs.go swapToVersion).
func (r *Repository) indexAlias(dim int) string {
	return fmt.Sprintf("%s_%d", r.baseIndex, dim)
}

// keywordsIndex returns the name of the dim-less keyword-only index used
// by the no-embedding save path (the service layer dispatches there when
// the knowledge base has vector indexing disabled). The mapping is the
// same as the per-dim index minus the knn_vector field.
func (r *Repository) keywordsIndex() string {
	return r.baseIndex + "_keywords"
}

// EngineType returns the engine constant for OpenSearch. Used by the
// fan-out normalizer to route to the [0, 1] passthrough group.
func (r *Repository) EngineType() types.RetrieverEngineType {
	return types.OpenSearchRetrieverEngineType
}

// Support reports the retrieval modes this driver implements. OpenSearch
// k-NN supports both ANN (vector) and BM25 (keyword) within a single
// document; the keyword-only index keeps BM25 functional for callers
// that omit the embedding map.
func (r *Repository) Support() []types.RetrieverType {
	return []types.RetrieverType{types.KeywordsRetrieverType, types.VectorRetrieverType}
}

// probeVersion sends GET / and validates the cluster is OpenSearch in
// a supported version range:
//   - Reject: ES (any), OS 1.x, OS 2.0~2.3 (Lucene HNSW preview).
//   - Warn-but-accept: OS 2.4~2.10 (Lucene HNSW GA, pre-LTS).
//   - Clean-accept: OS 2.11+, OS 3.x (primary tested: 3.3.2).
func probeVersion(ctx context.Context, client *osapi.Client) error {
	resp, err := client.Info(ctx, nil)
	if err != nil {
		return fmt.Errorf("opensearch: cluster info: %w", wrapTransport(err))
	}
	distribution := resp.Version.Distribution
	number := resp.Version.Number

	if distribution != "opensearch" {
		return fmt.Errorf("opensearch: unsupported distribution %q: %w",
			distribution, ErrVersionUnsupported)
	}
	maj, min := parseMajorMinor(number)
	switch {
	case maj == 1:
		return fmt.Errorf("opensearch: 1.x EOL: %w", ErrVersionUnsupported)
	case maj == 2 && min >= 0 && min <= 3:
		return fmt.Errorf("opensearch: %s lacks Lucene HNSW GA (need 2.4+): %w",
			number, ErrVersionUnsupported)
	case maj == 2 && min >= 4 && min <= 10:
		logger.GetLogger(ctx).Warnf(
			"[OpenSearch] using pre-2.11 cluster %s; recommend 2.11+ LTS", number)
		return nil
	case maj == 2 || maj == 3:
		return nil
	default:
		return fmt.Errorf("opensearch: unsupported version %s: %w",
			number, ErrVersionUnsupported)
	}
}

// parseMajorMinor extracts (major, minor) from a semver string such as
// "3.3.2", "2.10.0-SNAPSHOT", "2.10". Returns (0, 0) for unparseable
// input. Robust to pre-release suffixes (strips after '-') and missing
// patch component (e.g. "2.10").
func parseMajorMinor(num string) (major, minor int) {
	base := strings.SplitN(num, "-", 2)[0]
	parts := strings.Split(base, ".")
	if len(parts) < 2 {
		return 0, 0
	}
	m1, err1 := strconv.Atoi(parts[0])
	m2, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return m1, m2
}

// probeKNNPlugin verifies the OpenSearch cluster has the opensearch-knn
// plugin installed on EVERY node. Without it on at least one node,
// knn_vector queries that route a shard to that node fail with opaque
// shard-failure errors — we surface this as a clear "plugin missing on
// N nodes" message at registration.
//
// Implementation: _cat/plugins returns one row per (node, plugin) pair.
// We group by node name and verify every distinct node has the
// opensearch-knn component.
func probeKNNPlugin(ctx context.Context, client *osapi.Client) error {
	// CatPluginsReq's Params struct has no Format field — the SDK
	// negotiates JSON via Accept header and returns a typed
	// CatPluginsResp with .Plugins []CatPluginResp already parsed.
	resp, err := client.Cat.Plugins(ctx, &osapi.CatPluginsReq{})
	if err != nil {
		return fmt.Errorf("opensearch: cat plugins: %w", wrapTransport(err))
	}

	// Group by node, verify every distinct node has opensearch-knn.
	nodes := make(map[string]bool) // nodeName -> hasKNN
	for _, p := range resp.Plugins {
		if p.Name == "" {
			continue
		}
		if _, seen := nodes[p.Name]; !seen {
			nodes[p.Name] = false
		}
		if p.Component == "opensearch-knn" {
			nodes[p.Name] = true
		}
	}
	if len(nodes) == 0 {
		return fmt.Errorf("opensearch: _cat/plugins returned no rows: %w", ErrConfigInvalid)
	}
	var missing []string
	for node, hasKNN := range nodes {
		if !hasKNN {
			missing = append(missing, node)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"opensearch: opensearch-knn plugin missing on %d/%d nodes (%v): %w",
			len(missing), len(nodes), missing, ErrConfigInvalid,
		)
	}
	return nil
}

// wrapTransport classifies a raw transport error into one of the
// driver's sentinel errors based on HTTP status ONLY. The cluster-side
// error message (err.Error()) is intentionally NOT included in the
// wrapped public error — it may contain internal index names, shard
// IDs, node hostnames, or document content snippets. Raw SDK errors are not
// logged; only bounded sentinel classifications are exposed.
//
// Classification:
//   - 401 / 403  → ErrAuth
//   - 404        → caller decides (use isNotFound separately for index ops)
//   - 429 + knn_circuit_breaker_exception → ErrCircuitBreaker
//   - 5xx / network / timeout → ErrTransport
//
// This is the ONE place in the package that maps wire errors to
// sentinels; downstream functions just propagate.
func wrapTransport(err error) error {
	if err == nil {
		return nil
	}
	// Never log raw SDK errors: they can contain document bodies or credentials.
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, ErrRequestBudgetFull, ErrResponseTooLarge} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%w: %w", ErrTransport, sentinel)
		}
	}

	var se *opensearch.StructError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("authentication failed: %w", ErrAuth)
		case http.StatusTooManyRequests:
			if se.Err.Type == "knn_circuit_breaker_exception" {
				return fmt.Errorf("circuit breaker open: %w", ErrCircuitBreaker)
			}
		}
	}
	return fmt.Errorf("transport error: %w", ErrTransport)
}

// isNotFound returns true if err carries an HTTP 404. Used by index /
// search code paths that want to distinguish "missing alias" from
// "transport problem".
func isNotFound(err error) bool {
	var se *opensearch.StructError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

// isAlreadyExistsError returns true if err is a 400 with OpenSearch's
// resource_already_exists_exception type. Used by createIndexAndAlias
// to handle the cross-process race where two replicas try to create
// the same per-dim index simultaneously.
func isAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	var se *opensearch.StructError
	if !errors.As(err, &se) {
		return false
	}
	return se.Status == http.StatusBadRequest &&
		se.Err.Type == "resource_already_exists_exception"
}

// sanitizeRe enforces the OpenSearch index-name contract:
//   - lowercase ASCII letters, digits, underscore, hyphen only
//   - must start with [a-z0-9]
//   - length 1..255
var sanitizeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,254}$`)

// sanitizeIndexName rejects names that violate the OpenSearch index-name
// rules. Any non-conforming input returns ErrConfigInvalid — the driver
// does NOT silently rewrite invalid input so operators get a clear
// error at registration.
func sanitizeIndexName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty index name: %w", ErrConfigInvalid)
	}
	// Explicit reject of OpenSearch-reserved wildcards / separators /
	// control characters. Some of these would already be caught by the
	// regexp below, but a dedicated check keeps the error message
	// specific.
	if strings.ContainsAny(name, "*?,\n\r\t/\\") {
		return "", fmt.Errorf("invalid char in %q: %w", name, ErrConfigInvalid)
	}
	if !sanitizeRe.MatchString(name) {
		return "", fmt.Errorf("name %q must match %s: %w",
			name, sanitizeRe.String(), ErrConfigInvalid)
	}
	return name, nil
}

// drainAndClose releases the SDK's response view. Managed clients already
// consumed and closed the wire body in boundedRoundTripper; this remaining
// in-memory body is finite. Legacy injected clients retain their own contract.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

// limitedDecode wraps json.NewDecoder with an io.LimitReader so a
// pathological or malicious cluster response cannot exhaust process
// memory. Callers pass the documented per-endpoint cap (e.g. 64MB for
// bulk, 16MB for search, 1MB for plugin probe).
func limitedDecode(body io.Reader, maxBytes int64, v any) error {
	return json.NewDecoder(io.LimitReader(body, maxBytes)).Decode(v)
}
