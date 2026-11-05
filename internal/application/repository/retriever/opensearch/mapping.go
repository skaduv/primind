package opensearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/opensearch-project/opensearch-go/v4"
	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	"github.com/primind/primind/internal/logger"
)

// createIndexAndAlias creates "<alias>_v1" with the full k-NN mapping
// and then aliases "<alias>" → "<alias>_v1". Idempotent: returns nil if
// the alias already exists (so concurrent NewRepository calls in
// different processes don't race the PUT).
//
// Orphan cleanup: if aliasPut fails after a successful indicesCreate,
// the function attempts best-effort DELETE of the orphan _v1 index.
// Cleanup failure is logged but does not mask the original aliasPut
// error.
//
// Mapping-drift detection: if indicesCreate returns
// resource_already_exists_exception (cross-process race), the existing
// mapping's structural fingerprint is checked against the expected
// fingerprint. Drift returns ErrConfigInvalid wrapping a clear "manual
// reindex required" message instead of silently overlaying a foreign
// mapping.
func (r *Repository) createIndexAndAlias(ctx context.Context, dim int) error {
	log := logger.GetLogger(ctx)
	alias := r.indexAlias(dim)
	realIndex := alias + "_v1"

	exists, err := r.aliasExists(ctx, alias)
	if err != nil {
		return fmt.Errorf("alias check %s: %w", alias, err)
	}
	if exists {
		return nil // another writer already set it up
	}

	body, err := buildIndexMapping(r.cfg, dim)
	if err != nil {
		return fmt.Errorf("build mapping for dim=%d: %w", dim, err)
	}

	indexCreated := false
	if err := r.indicesCreate(ctx, realIndex, body); err != nil {
		if isAlreadyExistsError(err) {
			// Cross-process race or stale orphan. Verify the existing
			// mapping matches what we expected; drift → ErrConfigInvalid.
			if driftErr := r.verifyMappingMatches(ctx, realIndex, body); driftErr != nil {
				return fmt.Errorf(
					"mapping drift on %s: existing index has incompatible mapping; "+
						"manual reindex required: %w",
					realIndex, driftErr,
				)
			}
			// Mapping matches → safe to proceed with alias setup.
		} else {
			return fmt.Errorf("create index %s: %w", realIndex, err)
		}
	} else {
		indexCreated = true
	}

	if err := r.aliasPut(ctx, realIndex, alias); err != nil {
		// Best-effort orphan cleanup so future retries find a clean slate.
		if indexCreated {
			if delErr := r.indicesDelete(ctx, realIndex); delErr != nil {
				log.Warnf("[OpenSearch] orphan cleanup failed for %s: %v "+
					"(operator must DELETE manually)", realIndex, delErr)
			} else {
				log.Infof("[OpenSearch] cleaned up orphan %s after aliasPut failure", realIndex)
			}
		}
		return fmt.Errorf("put alias %s → %s: %w", alias, realIndex, err)
	}
	if indexCreated {
		// Emit only when we actually provisioned the index (not when a
		// concurrent writer / existing index short-circuited above).
		r.auditSink().EmitIndexCreated(ctx, alias, dim)
	}
	return nil
}

// buildIndexMapping returns the full mapping JSON for a new OpenSearch
// k-NN index. content uses the standard analyzer (BM25 default); all
// *_id fields are explicit keyword sub-fields so DeleteByQuery / terms
// filters hit them without the ES v8 "useKeywordSuffix" auto-detection
// dance.
//
// match_type is intentionally NOT in the mapping — it's a per-result
// classification derived from RetrieverType at parse time. source_type
// is stored as `integer` (not stringified) so the wire format remains
// stable across SourceType enum extension.
//
// Pure function — no IO, no logging. Easy to unit-test (byte-exact diff).
func buildIndexMapping(cfg internalCfg, dim int) ([]byte, error) {
	type mapping struct {
		Settings struct {
			Index struct {
				Knn                  bool   `json:"knn"`
				NumberOfShards       int    `json:"number_of_shards"`
				NumberOfReplicas     int    `json:"number_of_replicas"`
				RefreshInterval      string `json:"refresh_interval"`
				KnnAlgoParamEFSearch int    `json:"knn.algo_param.ef_search"`
			} `json:"index"`
		} `json:"settings"`
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	var m mapping
	m.Settings.Index.Knn = true
	m.Settings.Index.NumberOfShards = cfg.shards
	m.Settings.Index.NumberOfReplicas = cfg.replicas
	m.Settings.Index.RefreshInterval = "1s"
	m.Settings.Index.KnnAlgoParamEFSearch = cfg.efSearch
	m.Mappings.Properties = map[string]any{
		"embedding": map[string]any{
			"type":      "knn_vector",
			"dimension": dim,
			"method": map[string]any{
				"name":       "hnsw",
				"space_type": "cosinesimil",
				"engine":     cfg.knnEngine,
				"parameters": map[string]any{
					"m":               cfg.hnswM,
					"ef_construction": cfg.hnswEFConstruction,
				},
			},
		},
		"content":           map[string]any{"type": "text", "analyzer": "standard"},
		"chunk_id":          map[string]any{"type": "keyword"},
		"knowledge_id":      map[string]any{"type": "keyword"},
		"knowledge_base_id": map[string]any{"type": "keyword"},
		"tag_id":            map[string]any{"type": "keyword"},
		"source_id":         map[string]any{"type": "keyword"},
		"source_type":       map[string]any{"type": "integer"},
		"is_enabled":        map[string]any{"type": "boolean"},
		"is_recommended":    map[string]any{"type": "boolean"},
	}
	out, err := json.Marshal(&m)
	if err != nil {
		// Cannot happen for our internal struct, but propagate for symmetry
		// with the other JSON builders in the package.
		return nil, fmt.Errorf("marshal mapping: %w", err)
	}
	return out, nil
}

// buildKeywordsMapping returns the mapping for the dim-less keyword-only
// index. Same as buildIndexMapping but with the embedding field omitted
// — used by the no-embedding save path (the service layer dispatches
// there when the knowledge base has vector indexing disabled).
func buildKeywordsMapping(cfg internalCfg) ([]byte, error) {
	type mapping struct {
		Settings struct {
			Index struct {
				NumberOfShards   int    `json:"number_of_shards"`
				NumberOfReplicas int    `json:"number_of_replicas"`
				RefreshInterval  string `json:"refresh_interval"`
			} `json:"index"`
		} `json:"settings"`
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	var m mapping
	m.Settings.Index.NumberOfShards = cfg.shards
	m.Settings.Index.NumberOfReplicas = cfg.replicas
	m.Settings.Index.RefreshInterval = "1s"
	m.Mappings.Properties = map[string]any{
		"content":           map[string]any{"type": "text", "analyzer": "standard"},
		"chunk_id":          map[string]any{"type": "keyword"},
		"knowledge_id":      map[string]any{"type": "keyword"},
		"knowledge_base_id": map[string]any{"type": "keyword"},
		"tag_id":            map[string]any{"type": "keyword"},
		"source_id":         map[string]any{"type": "keyword"},
		"source_type":       map[string]any{"type": "integer"},
		"is_enabled":        map[string]any{"type": "boolean"},
		"is_recommended":    map[string]any{"type": "boolean"},
	}
	out, err := json.Marshal(&m)
	if err != nil {
		return nil, fmt.Errorf("marshal keywords mapping: %w", err)
	}
	return out, nil
}

// ensureKeywordsIndex uses the shared initialization-attempt mechanism for
// the dim-less index. The ready flag is a fast path after successful creation.
func (r *Repository) ensureKeywordsIndex(ctx context.Context) error {
	ctx, cancel := operationContext(ctx, r.client)
	defer cancel()
	r.keywordsMu.Lock()
	ready := r.keywordsReady
	r.keywordsMu.Unlock()
	if ready {
		return ctx.Err()
	}
	return r.ensureIndex(ctx, 0, r.createKeywordsIndex)
}

func (r *Repository) createKeywordsIndex(ctx context.Context) error {
	name := r.keywordsIndex()
	resp, requestErr := r.client.Client.Do(ctx, osapi.IndicesExistsReq{Indices: []string{name}}, nil)
	exists, err := existenceResponse(resp, requestErr)
	if err != nil {
		return err
	}
	if exists {
		r.keywordsMu.Lock()
		r.keywordsReady = true
		r.keywordsMu.Unlock()
		return nil
	}
	body, err := buildKeywordsMapping(r.cfg)
	if err != nil {
		return err
	}
	created := false
	if err := r.indicesCreate(ctx, name, body); err != nil {
		if !isAlreadyExistsError(err) {
			return err
		}
		// resource_already_exists_exception — race with concurrent process,
		// treat as success.
	} else {
		created = true
	}
	r.keywordsMu.Lock()
	r.keywordsReady = true
	r.keywordsMu.Unlock()
	if created {
		// dim=0 marks the dim-less keyword-only index.
		r.auditSink().EmitIndexCreated(ctx, name, 0)
	}
	return nil
}

// indicesCreate is the low-level wrapper around Indices.Create. Body is
// the JSON mapping produced by buildIndexMapping.
func (r *Repository) indicesCreate(ctx context.Context, name string, body []byte) error {
	req := osapi.IndicesCreateReq{Index: name, Body: bytes.NewReader(body)}
	resp, err := r.client.Indices.Create(ctx, req)
	if err != nil {
		return wrapTransport(err)
	}
	if resp != nil {
		drainAndClose(resp.Inspect().Response.Body)
	}
	return nil
}

// indicesDelete is used by createIndexAndAlias's orphan cleanup path.
// Caller decides what to do with the error.
func (r *Repository) indicesDelete(ctx context.Context, name string) error {
	req := osapi.IndicesDeleteReq{Indices: []string{name}}
	resp, err := r.client.Indices.Delete(ctx, req)
	if err != nil {
		return wrapTransport(err)
	}
	if resp != nil {
		drainAndClose(resp.Inspect().Response.Body)
	}
	return nil
}

// aliasExistsRequest avoids SDK v4.6.0's empty-Indices // _alias path
// (without the space), which net/http parses as an authority. Only this
// request's construction changes; Client.Do retains the configured transport,
// endpoint prefix, authentication, SSRF checks, runtime budget and context.
type aliasExistsRequest string

func (alias aliasExistsRequest) GetRequest() (*http.Request, error) {
	return opensearch.BuildRequest(http.MethodHead, "/_alias/"+string(alias), nil, nil, nil)
}

func (r *Repository) aliasExists(ctx context.Context, alias string) (bool, error) {
	resp, err := r.client.Client.Do(ctx, aliasExistsRequest(alias), nil)
	return existenceResponse(resp, err)
}

// HEAD responses have no error document to decode. Classify the status through
// the same sentinel mapper as other requests, accepting only 200 and 404.
func existenceResponse(resp *opensearch.Response, err error) (bool, error) {
	if resp != nil {
		defer drainAndClose(resp.Body)
	}
	if err != nil {
		return false, wrapTransport(err)
	}
	if resp == nil {
		return false, wrapTransport(fmt.Errorf("missing existence response"))
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, wrapTransport(&opensearch.StructError{Status: resp.StatusCode})
	}
}

// aliasPut creates an alias pointing at index. Single add operation
// (rolling-reindex atomic swap is deferred to a follow-up PR — see
// stubs.go swapToVersion).
func (r *Repository) aliasPut(ctx context.Context, index, alias string) error {
	req := osapi.AliasPutReq{
		Indices: []string{index},
		Alias:   alias,
	}
	resp, err := r.client.Indices.Alias.Put(ctx, req)
	if err != nil {
		return wrapTransport(err)
	}
	if resp != nil {
		drainAndClose(resp.Inspect().Response.Body)
	}
	return nil
}

// verifyMappingMatches GETs the existing index's mapping and compares
// the embedding field's structural fingerprint (dimension, HNSW
// parameters, engine) against the expected. A full field-by-field diff
// would be ideal but expensive; we check only the parts that matter for
// query correctness.
func (r *Repository) verifyMappingMatches(ctx context.Context, index string, expected []byte) error {
	req := osapi.MappingGetReq{Indices: []string{index}}
	resp, err := r.client.Indices.Mapping.Get(ctx, &req)
	if err != nil {
		return fmt.Errorf("get mapping %s: %w", index, wrapTransport(err))
	}
	if resp != nil {
		drainAndClose(resp.Inspect().Response.Body)
	}

	// Both fingerprints are derived from the mapping's embedding-field
	// HNSW parameters: dimension, m, ef_construction, engine, space_type.
	// We parse the expected (our own JSON) and the actual (cluster
	// response) and compare just those bits.
	expectedFP, err := extractEmbeddingFingerprint(expected)
	if err != nil {
		return fmt.Errorf("parse expected fingerprint: %w", err)
	}
	actualFP, err := extractMappingFingerprintFromResp(resp, index)
	if err != nil {
		return fmt.Errorf("parse actual fingerprint: %w", err)
	}
	if expectedFP != actualFP {
		return fmt.Errorf("mapping fingerprint mismatch: expected %+v, got %+v: %w",
			expectedFP, actualFP, ErrConfigInvalid)
	}
	return nil
}

// embFingerprint is the minimal set of HNSW parameters that uniquely
// determine query semantics for a k-NN index. If any of these differ
// between create-time and an existing index, retrieval is incorrect.
type embFingerprint struct {
	Dimension      int
	M              int
	EFConstruction int
	Engine         string
	SpaceType      string
}

// extractEmbeddingFingerprint parses our own mapping JSON (produced by
// buildIndexMapping) to pull out the HNSW parameters.
func extractEmbeddingFingerprint(body []byte) (embFingerprint, error) {
	var m struct {
		Mappings struct {
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return embFingerprint{}, err
	}
	return extractFingerprintFromProps(m.Mappings.Properties)
}

// extractMappingFingerprintFromResp parses the cluster's GET-mapping
// response and pulls out the embedding-field HNSW fingerprint for the
// given index.
func extractMappingFingerprintFromResp(resp *osapi.MappingGetResp, index string) (embFingerprint, error) {
	indexMapping, ok := resp.Indices[index]
	if !ok {
		return embFingerprint{}, fmt.Errorf("index %s not in mapping response", index)
	}
	// MappingGetResp.Indices[index].Mappings is a json.RawMessage in
	// the SDK — we decode the inner properties shape ourselves.
	var m struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(indexMapping.Mappings, &m); err != nil {
		return embFingerprint{}, err
	}
	return extractFingerprintFromProps(m.Properties)
}

func extractFingerprintFromProps(props map[string]any) (embFingerprint, error) {
	emb, ok := props["embedding"].(map[string]any)
	if !ok {
		return embFingerprint{}, fmt.Errorf("embedding field missing or wrong type")
	}
	dim, _ := emb["dimension"].(float64) // json.Unmarshal produces float64 for numbers
	method, _ := emb["method"].(map[string]any)
	engine, _ := method["engine"].(string)
	spaceType, _ := method["space_type"].(string)
	params, _ := method["parameters"].(map[string]any)
	m, _ := params["m"].(float64)
	efc, _ := params["ef_construction"].(float64)
	return embFingerprint{
		Dimension:      int(dim),
		M:              int(m),
		EFConstruction: int(efc),
		Engine:         engine,
		SpaceType:      spaceType,
	}, nil
}
