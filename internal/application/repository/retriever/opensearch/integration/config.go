// Package integration contains the OS-02 opt-in verification harness. It is
// deliberately separate from the adapter's loopback-only TestMain.
package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/primind/primind/internal/types"
)

var errNotRun = errors.New("NOT_RUN: explicit OS-02 opt-in and dedicated config required")
var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,180}$`)
var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,180}$`)
var secretName = regexp.MustCompile(`^OS02_SECRET_[A-Z0-9_]+$`)

type Config struct {
	Mode                string       `json:"mode"`
	RunID               string       `json:"run_id"`
	AuthorizationRef    string       `json:"authorization_ref"`
	Expires             time.Time    `json:"expires"`
	Endpoint            string       `json:"endpoint"`
	ClusterUUID         string       `json:"cluster_uuid"`
	Version             string       `json:"version"`
	UsernameEnv         string       `json:"username_env"`
	PasswordEnv         string       `json:"password_env"`
	AnonymousAuthorized bool         `json:"anonymous_authorized"`
	InsecureSkipVerify  bool         `json:"insecure_skip_verify"`
	TLSExceptionRef     string       `json:"tls_exception_ref"`
	QuiescentScope      bool         `json:"quiescent_scope"`
	MaxSeconds          int          `json:"max_seconds"`
	VisibilitySeconds   int          `json:"visibility_seconds"`
	PollMilliseconds    int          `json:"poll_milliseconds"`
	PageSize            int          `json:"page_size"`
	MaxRecords          int          `json:"max_records"`
	TotalRequests       int          `json:"total_requests"`
	WriteRequests       int          `json:"write_requests"`
	Waiters             int          `json:"waiters"`
	Replay              *Scope       `json:"replay,omitempty"`
	Performance         *Performance `json:"performance,omitempty"`
}

// IndexConfig must be the explicitly approved effective configuration. Requiring
// nonzero effective values prevents adapter env/default fallback, including the
// existing replicas=0 -> 1 projection; it does not repair that public DTO here.
type Scope struct {
	StoreID         string            `json:"store_id"`
	Index           types.IndexConfig `json:"index_config"`
	Alias           string            `json:"alias"`
	Physical        string            `json:"physical"`
	Dimension       int               `json:"dimension"`
	KnowledgeBaseID string            `json:"knowledge_base_id"`
	KnowledgeID     string            `json:"knowledge_id"`
	ChunkID         string            `json:"chunk_id,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	RefreshInterval string            `json:"refresh_interval"`
}

type Performance struct {
	AllowIndexCreationAndOrphanCleanup bool         `json:"allow_adapter_index_creation_and_orphan_cleanup"`
	ProfileRef                         string       `json:"profile_ref"`
	BudgetRef                          string       `json:"budget_ref"`
	Seed                               string       `json:"seed"`
	MaxDocuments                       int          `json:"max_documents"`
	MaxConcurrency                     int          `json:"max_concurrency"`
	MaxHeapBytes                       uint64       `json:"max_heap_bytes"`
	HealthFile                         string       `json:"health_file"`
	HealthMaxAgeSeconds                int          `json:"health_max_age_seconds"`
	Rounds                             []Round      `json:"rounds"`
	Application                        *Application `json:"application,omitempty"`
}

type Round struct {
	ID                        string  `json:"id"`
	Variant                   string  `json:"variant"`
	Warmup                    bool    `json:"warmup"`
	Mixed                     bool    `json:"mixed"`
	BatchSize                 int     `json:"batch_size"`
	Concurrency               int     `json:"concurrency"`
	DocumentsPerStore         int     `json:"documents_per_store"`
	MinimumSeconds            int     `json:"minimum_seconds"`
	MinimumQueries            int     `json:"minimum_queries"`
	QueryIntervalMilliseconds int     `json:"query_interval_milliseconds"`
	Stores                    []Scope `json:"stores"`
}

type Application struct {
	Endpoint          string   `json:"endpoint"`
	TokenEnv          string   `json:"token_env"`
	KnowledgeBaseID   string   `json:"knowledge_base_id"`
	KnowledgeAID      string   `json:"knowledge_a_id"`
	EvidenceAChunkIDs []string `json:"evidence_a_chunk_ids"`
	MatchCount        int      `json:"match_count"`
	VectorThreshold   float64  `json:"vector_threshold"`
	KeywordThreshold  float64  `json:"keyword_threshold"`
}

// loadConfig checks the opt-in before reading any file, secret, endpoint or
// service environment. Empty/missing config is never an implicit local service.
func loadConfig(mode string, getenv func(string) string) (Config, error) {
	if getenv("PRIMIND_OS02_OPT_IN") != "OS-02:"+mode || getenv("PRIMIND_OS02_CONFIG") == "" {
		return Config{}, errNotRun
	}
	b, err := readLimited(getenv("PRIMIND_OS02_CONFIG"), 1<<20)
	if err != nil {
		return Config{}, errors.New("dedicated config cannot be read")
	}
	var c Config
	if err := strictJSON(b, &c); err != nil {
		return c, errors.New("invalid dedicated config JSON")
	}
	if c.Mode != mode {
		return c, errors.New("opt-in and config mode differ")
	}
	return c, c.validate(time.Now())
}

func strictJSON(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func readLimited(path string, cap int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, cap+1))
	if int64(len(b)) > cap {
		return nil, errors.New("file exceeds cap")
	}
	return b, err
}

func validateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		strings.ContainsAny(u.Host, "*,\r\n ") {
		return errors.New("endpoint must be one exact HTTP(S) origin without credentials")
	}
	return nil
}

func (c Config) validate(now time.Time) error {
	if !safeName.MatchString(c.RunID) || c.AuthorizationRef == "" || !c.Expires.After(now) ||
		c.Expires.After(now.Add(24*time.Hour)) || !c.QuiescentScope || c.ClusterUUID == "" || c.Version != "2.19.7" {
		return errors.New("run identity, current authorization, quiescent scope and pinned cluster/version required")
	}
	if err := validateEndpoint(c.Endpoint); err != nil {
		return err
	}
	if c.InsecureSkipVerify && c.TLSExceptionRef == "" {
		return errors.New("TLS exception requires explicit reference")
	}
	if c.AnonymousAuthorized {
		if c.UsernameEnv != "" || c.PasswordEnv != "" {
			return errors.New("ambiguous credentials")
		}
	} else if !secretName.MatchString(c.UsernameEnv) || !secretName.MatchString(c.PasswordEnv) {
		return errors.New("dedicated credential environment references required")
	}
	if c.MaxSeconds < 1 || c.MaxSeconds > 3600 || c.VisibilitySeconds < 1 || c.VisibilitySeconds > 300 ||
		c.PollMilliseconds < 10 || c.PollMilliseconds > 10000 || c.PollMilliseconds > c.VisibilitySeconds*1000 || c.PageSize < 1 || c.PageSize > 100 ||
		c.MaxRecords < 1 || c.MaxRecords > 100000 || c.TotalRequests < 2 || c.TotalRequests > 16 ||
		c.WriteRequests < 1 || c.WriteRequests >= c.TotalRequests || c.Waiters < 1 || c.Waiters > 64 {
		return errors.New("finite local runner limits required (never larger than approved adapter budget)")
	}
	if time.Duration(c.MaxSeconds)*time.Second > c.Expires.Sub(now) {
		return errors.New("run exceeds authorization window")
	}
	switch c.Mode {
	case "replay":
		if c.Replay == nil || c.Performance != nil {
			return errors.New("exactly one replay scope required")
		}
		if err := c.Replay.validate(); err != nil {
			return err
		}
		if c.Replay.Dimension < 1 || !safeID.MatchString(c.Replay.ChunkID) || c.Replay.Namespace != "" {
			return errors.New("existing vector chunk required")
		}
	case "performance":
		if c.Performance == nil || c.Replay != nil {
			return errors.New("performance profile required")
		}
		return c.validatePerformance()
	default:
		return errors.New("unknown mode")
	}
	return nil
}

func (s Scope) validate() error {
	i := s.Index
	if !safeID.MatchString(s.StoreID) || len(s.StoreID) < 16 || !safeName.MatchString(i.IndexName) ||
		!safeID.MatchString(s.KnowledgeBaseID) || !safeID.MatchString(s.KnowledgeID) || s.Dimension < 0 || s.Dimension > 16000 ||
		i.NumberOfShards < 1 || i.NumberOfReplicas < 1 || i.HNSWM < 1 || i.HNSWEFConstruction < 1 || i.HNSWEFSearch < 1 ||
		(i.KNNEngine != "lucene" && i.KNNEngine != "faiss") || s.RefreshInterval == "" {
		return errors.New("incomplete explicit scope/index configuration")
	}
	base := i.IndexName + "_" + s.StoreID[:12]
	expected := fmt.Sprintf("%s_%d", base, s.Dimension)
	physical := expected + "_v1"
	if s.Dimension == 0 {
		expected = base + "_keywords"
		physical = expected
	}
	if s.Alias != expected || s.Physical != physical || !safeName.MatchString(s.Alias) || !safeName.MatchString(s.Physical) {
		return errors.New("alias/physical target differs from formal adapter routing")
	}
	return nil
}

func (s Scope) keywordIndex() string { return s.Index.IndexName + "_" + s.StoreID[:12] + "_keywords" }

func (c Config) validatePerformance() error {
	p := c.Performance
	if !p.AllowIndexCreationAndOrphanCleanup || p.ProfileRef == "" || p.BudgetRef == "" || p.Seed == "" || p.MaxDocuments < 1 || p.MaxDocuments > c.MaxRecords ||
		p.MaxConcurrency < 1 || p.MaxConcurrency > c.WriteRequests || p.MaxHeapBytes == 0 || p.HealthFile == "" ||
		p.HealthMaxAgeSeconds < 1 || p.HealthMaxAgeSeconds > 30 || len(p.Rounds) == 0 || len(p.Rounds) > 40 {
		return errors.New("frozen workload, finite budget and approved fresh health feed required")
	}
	seen := map[string]bool{}
	signatures := map[string]string{}
	repeats := map[string]int{}
	profiles := map[string]string{}
	baselines := map[string]bool{}
	candidates := map[string]bool{}
	total := 0
	baseline, candidate, mixed := false, false, false
	for _, r := range p.Rounds {
		if !safeName.MatchString(r.ID) || !safeName.MatchString(r.Variant) || seen["round:"+r.ID] ||
			r.BatchSize < 1 || r.BatchSize > 1000 || r.Concurrency < 1 || r.Concurrency > p.MaxConcurrency ||
			r.DocumentsPerStore < r.BatchSize*r.Concurrency+1 || len(r.Stores) < 2 || len(r.Stores) > 4 ||
			r.MinimumSeconds < 0 || r.MinimumSeconds > c.MaxSeconds || r.QueryIntervalMilliseconds < 10 ||
			r.QueryIntervalMilliseconds > 10000 || (r.Mixed && (r.MinimumSeconds < 1 || r.MinimumQueries < 3)) {
			return errors.New("invalid finite round/multi-store/minimum workload")
		}
		seen["round:"+r.ID] = true
		sig := fmt.Sprintf("%d/%d/%d/%t/%d/%d", r.BatchSize, r.Concurrency, r.DocumentsPerStore, r.Mixed, r.MinimumSeconds, r.MinimumQueries)
		if old, ok := signatures[r.Variant]; ok && old != sig {
			return errors.New("variant changed across repetitions")
		}
		signatures[r.Variant] = sig
		if !r.Warmup {
			repeats[r.Variant]++
			baseline = baseline || r.Concurrency == 1
			candidate = candidate || r.Concurrency == 2 || r.Concurrency == 4
			mixed = mixed || r.Mixed
		}
		for _, s := range r.Stores {
			if err := s.validate(); err != nil {
				return err
			}
			if s.RefreshInterval != "1s" {
				return errors.New("formal adapter creates indexes with 1s refresh; no tuning override allowed")
			}
			if !r.Warmup && r.Concurrency == 1 && ((s.Dimension > 0 && r.BatchSize != 40) || (s.Dimension == 0 && r.BatchSize != 10)) {
				return errors.New("single-worker baseline preserves existing vector 40 / keyword 10 batch")
			}
			prefix := "os02perf_" + c.RunID + "_"
			if !strings.HasPrefix(s.Index.IndexName, prefix) || !strings.HasPrefix(s.Namespace, prefix) ||
				s.ChunkID != "" || s.KnowledgeBaseID != s.Namespace+"_kb" || s.KnowledgeID != s.Namespace+"_doc" ||
				seen["index:"+s.Alias] || seen["namespace:"+s.Namespace] || seen["store:"+r.ID+":"+s.StoreID] || (r.Mixed && s.Dimension < 1) {
				return errors.New("fresh dedicated synthetic scopes required; application chunks are forbidden")
			}
			seen["index:"+s.Alias] = true
			seen["namespace:"+s.Namespace] = true
			seen["store:"+r.ID+":"+s.StoreID] = true
			total += r.DocumentsPerStore
		}
		var datasetBytes uint64
		for _, s := range r.Stores {
			datasetBytes += uint64(r.DocumentsPerStore) * (uint64(s.Dimension)*4 + 4096)
		}
		if datasetBytes > p.MaxHeapBytes/2 {
			return errors.New("planned dataset exceeds conservative half-heap envelope before allocation")
		}
		// Compare repetitions independently of disposable names/IDs. A candidate
		// keeps documents, dimensions and index settings identical to its baseline;
		// only concurrency changes in this deliberately small first matrix.
		shape := make([]any, 0, len(r.Stores))
		for _, s := range r.Stores {
			index := s.Index
			index.IndexName = ""
			shape = append(shape, struct {
				Index     types.IndexConfig
				Dimension int
				Refresh   string
			}{index, s.Dimension, s.RefreshInterval})
		}
		b, _ := json.Marshal(struct {
			Batch, Documents int
			Shape            []any
		}{r.BatchSize, r.DocumentsPerStore, shape})
		profile := string(b)
		if old, ok := profiles[r.Variant]; ok && old != profile {
			return errors.New("variant index/dimension/load profile changed")
		}
		profiles[r.Variant] = profile
		if !r.Warmup && !r.Mixed {
			if r.Concurrency == 1 {
				baselines[profile] = true
			} else {
				candidates[profile] = true
			}
		}
	}
	if total > p.MaxDocuments || !baseline || !candidate || !mixed {
		return errors.New("budget or baseline/candidate/mixed matrix incomplete")
	}
	for _, n := range repeats {
		if n < 3 {
			return errors.New("each measured variant requires at least three independent rounds")
		}
	}
	if len(candidates) == 0 {
		return errors.New("measured pure-write candidate required")
	}
	for profile := range candidates {
		if !baselines[profile] {
			return errors.New("candidate has no otherwise identical single-worker baseline")
		}
	}
	if p.Application == nil {
		return errors.New("mixed round needs authorized application hybrid probes; adapter has no hybrid method")
	}
	a := p.Application
	if err := validateEndpoint(a.Endpoint); err != nil {
		return err
	}
	if !secretName.MatchString(a.TokenEnv) || !safeID.MatchString(a.KnowledgeBaseID) || !safeID.MatchString(a.KnowledgeAID) ||
		len(a.EvidenceAChunkIDs) == 0 || a.MatchCount < 1 || a.MatchCount > 100 || a.VectorThreshold < -1 || a.VectorThreshold > 1 || a.KeywordThreshold < 0 {
		return errors.New("application scope, frozen retrieval parameters and independent A evidence IDs required")
	}
	for _, id := range a.EvidenceAChunkIDs {
		if !safeID.MatchString(id) {
			return errors.New("invalid application evidence ID")
		}
	}
	return nil
}
