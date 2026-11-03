package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	osapi "github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	"github.com/primind/primind/internal/types"
)

// Synthetic vectors are used only in dedicated pure-index namespaces. The
// separate application lane uses the real model and Q-KW/Q-VEC/Q-HYB oracle.
func synthetic(s Scope, seed string, n int) Document {
	id := fmt.Sprintf("%s_c%06d", s.Namespace, n)
	d := Document{ChunkID: id, SourceID: fmt.Sprintf("%s_source%06d", s.Namespace, n), KnowledgeBaseID: s.KnowledgeBaseID,
		KnowledgeID: s.KnowledgeID, TagID: s.Namespace + "_tag", Content: fmt.Sprintf("OS02PERF %s item %06d 合成索引负载 \"quoted\"\n", s.Namespace, n), Enabled: true, Recommended: n%2 == 0}
	for j := 0; j < n%5; j++ {
		d.Content += "固定长度分布样本 "
	}
	if s.Dimension > 0 {
		d.Embedding = make([]float32, s.Dimension)
		for i := range d.Embedding {
			h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", seed, n, i)))
			d.Embedding[i] = float32(int32(binary.LittleEndian.Uint32(h[:4]))%10001) / 10001
		}
		d.Embedding[0] = 0.5 // nonzero cosine vector, deterministic and finite
	}
	return d
}

type HealthSample struct {
	RunID                          string    `json:"run_id"`
	BudgetRef                      string    `json:"budget_ref"`
	At                             time.Time `json:"at"`
	Stop                           bool      `json:"stop"`
	HostCPUPercent                 float64   `json:"host_cpu_percent"`
	HostAvailableBytes             uint64    `json:"host_available_bytes"`
	DiskFreeBytes                  uint64    `json:"disk_free_bytes"`
	OSRSSBytes                     uint64    `json:"os_rss_bytes"`
	OSHeapBytes                    uint64    `json:"os_heap_bytes"`
	OSWriteQueue                   int       `json:"os_write_queue"`
	OSRejectedDelta                int       `json:"os_rejected_delta"`
	ExistingServiceP95Milliseconds float64   `json:"existing_service_p95_milliseconds"`
}

// The approved external observer supplies host/OS/existing-service readings and
// evaluates the frozen stop lines in budget_ref. This harness does not infer
// permission to scrape nodes, inspect processes or run system commands.
func health(c Config) (HealthSample, error) {
	p := c.Performance
	var h HealthSample
	b, err := readLimited(p.HealthFile, 16<<10)
	if err != nil {
		return h, errors.New("approved health feed unavailable")
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(b, &fields); err != nil {
		return h, errors.New("invalid health feed")
	}
	for _, key := range []string{"run_id", "budget_ref", "at", "stop", "host_cpu_percent", "host_available_bytes", "disk_free_bytes", "os_rss_bytes", "os_heap_bytes", "os_write_queue", "os_rejected_delta", "existing_service_p95_milliseconds"} {
		value, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return h, fmt.Errorf("required health observation %s absent or null", key)
		}
	}
	if err = strictJSON(b, &h); err != nil {
		return h, errors.New("invalid health feed")
	}
	age := time.Since(h.At)
	if h.RunID != c.RunID || h.BudgetRef != p.BudgetRef || age < 0 || age > time.Duration(p.HealthMaxAgeSeconds)*time.Second || h.Stop || h.OSRejectedDelta != 0 ||
		h.HostCPUPercent < 0 || h.HostCPUPercent > 100 || h.HostAvailableBytes == 0 || h.DiskFreeBytes == 0 || h.OSRSSBytes == 0 || h.OSHeapBytes == 0 || h.OSWriteQueue < 0 || h.ExistingServiceP95Milliseconds < 0 {
		return h, errors.New("health feed stale, incomplete, out of scope or stop line reached")
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if mem.HeapAlloc > p.MaxHeapBytes {
		return h, errors.New("runner heap stop line reached")
	}
	return h, nil
}

type Sample struct {
	Kind    string  `json:"kind"`
	StoreID string  `json:"store_id,omitempty"`
	Seconds float64 `json:"seconds"`
	Status  string  `json:"status"`
	Items   int     `json:"items,omitempty"`
}

type RoundResult struct {
	ID                     string                        `json:"id"`
	Status                 string                        `json:"status"`
	Warmup                 bool                          `json:"warmup"`
	PacedMixed             bool                          `json:"paced_mixed"`
	WallSeconds            float64                       `json:"write_wall_seconds"`
	VisibilitySeconds      float64                       `json:"post_write_visibility_seconds"`
	CorrectUnique          int                           `json:"correct_unique"`
	CorrectUniquePerSecond float64                       `json:"correct_unique_per_second"`
	BulkBytes              int64                         `json:"bulk_ndjson_bytes"`
	BulkObservations       []adapter.WriteObservation    `json:"bulk_observations"`
	Samples                []Sample                      `json:"samples"`
	ApplicationEvidence    []ApplicationEvidence         `json:"application_evidence"`
	Quantiles              map[string]map[string]float64 `json:"quantiles_seconds"`
	Manifests              map[string][]RecordDigest     `json:"expected_manifest"`
	ActualManifests        map[string][]RecordDigest     `json:"actual_manifest"`
	Budget                 adapter.BudgetSnapshot        `json:"budget"`
	Connections            map[string]int                `json:"connections"`
	Health                 []HealthSample                `json:"health"`
	HealthFailure          string                        `json:"health_failure,omitempty"`
	HeapBefore, HeapAfter  uint64
	GCBefore, GCAfter      uint32
}

type PerformanceResult struct {
	RunID                     string        `json:"run_id"`
	Status                    string        `json:"status"`
	Rounds                    []RoundResult `json:"rounds"`
	ApplicationPipelineTiming string        `json:"application_pipeline_timing"`
	BusinessSLA               string        `json:"business_sla"`
}

func summarize(samples []Sample) map[string]map[string]float64 {
	groups := map[string][]float64{}
	for _, s := range samples {
		if s.Status == "PASS" {
			groups[s.Kind] = append(groups[s.Kind], s.Seconds)
		}
	}
	out := map[string]map[string]float64{}
	for name, values := range groups {
		sort.Float64s(values)
		q := func(p float64) float64 { i := max(0, int(math.Ceil(float64(len(values))*p))-1); return values[i] }
		out[name] = map[string]float64{"n": float64(len(values)), "p50": q(.5), "p95": q(.95)}
		if len(values) >= 100 {
			out[name]["p99"] = q(.99)
		}
	}
	return out
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type job struct {
	store int
	docs  []Document
	at    time.Time
}

// dispatch has a fixed worker count and an unbuffered queue, not a goroutine per
// batch. The first error cancels admission and all peers, then joins them. SDK
// and parent contexts bound transport/body/retry/drain; no write is replayed here.
func dispatch(ctx context.Context, cancel context.CancelFunc, concurrency int, jobs []job, write func(context.Context, job) error) error {
	queue := make(chan job)
	first := make(chan error, 1)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				if ctx.Err() != nil {
					return
				}
				if err := sleepContext(ctx, time.Until(j.at)); err != nil {
					return
				}
				if ctx.Err() != nil {
					return
				}
				if err := write(ctx, j); err != nil {
					select {
					case first <- err:
					default:
					}
					cancel()
					return
				}
			}
		}()
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case queue <- j:
		}
	}
	close(queue)
	wg.Wait()
	select {
	case err := <-first:
		return err
	default:
		return ctx.Err()
	}
}

func verifyRetrieval(results []*types.RetrieveResult, scope Scope, expected map[string]Document) error {
	found := false
	seen := map[string]bool{}
	for _, bundle := range results {
		if bundle == nil || bundle.Error != nil {
			return errors.New("partial retrieval")
		}
		for _, hit := range bundle.Results {
			if hit == nil {
				return errors.New("nil retrieval hit")
			}
			d, ok := expected[hit.ChunkID]
			if !ok || seen[hit.ChunkID] || hit.KnowledgeBaseID != scope.KnowledgeBaseID || hit.KnowledgeID != scope.KnowledgeID || hit.SourceID != d.SourceID || hit.Content != d.Content || !hit.IsEnabled {
				return errors.New("retrieval escaped independent manifest or duplicated chunk")
			}
			seen[hit.ChunkID] = true
			found = true
		}
	}
	if !found {
		return errors.New("fixed anchor not retrieved")
	}
	return nil
}

func runPerformance(parent context.Context, c Config) (result PerformanceResult, err error) {
	result = PerformanceResult{RunID: c.RunID, Status: "FAIL", ApplicationPipelineTiming: "NOT_RUN: collect authorized application spans separately", BusinessSLA: "NOT_EVALUATED"}
	if err = c.validate(time.Now()); err != nil {
		return result, err
	}
	if c.Mode != "performance" {
		return result, errors.New("performance mode required")
	}
	if _, err = health(c); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(c.MaxSeconds)*time.Second)
	defer cancel()
	for _, round := range c.Performance.Rounds {
		r, e := runRound(ctx, c, round)
		result.Rounds = append(result.Rounds, r)
		if e != nil {
			return result, e
		}
	}
	result.Status = "INDEX_AND_RETRIEVAL_PASS_E2_OTHER_GATES_PENDING"
	return result, nil
}

func runRound(parent context.Context, c Config, round Round) (result RoundResult, err error) {
	result = RoundResult{ID: round.ID, Status: "FAIL", Warmup: round.Warmup, PacedMixed: round.Mixed, Manifests: map[string][]RecordDigest{}, ActualManifests: map[string][]RecordDigest{}}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stats := &connectionStats{}
	ctx = stats.context(ctx)
	budget, _ := adapter.NewRequestBudget(c.TotalRequests, c.WriteRequests, c.Waiters)
	var mu sync.Mutex
	var clients []*osapi.Client
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	result.HeapBefore = mem.HeapAlloc
	result.GCBefore = mem.NumGC
	defer func() {
		closeClients(clients)
		result.Budget = budget.Snapshot()
		result.Connections = stats.snapshot()
		runtime.ReadMemStats(&mem)
		result.HeapAfter = mem.HeapAlloc
		result.GCAfter = mem.NumGC
		result.Quantiles = summarize(result.Samples)
	}()
	observer := func(o adapter.WriteObservation) {
		mu.Lock()
		defer mu.Unlock()
		result.BulkBytes += int64(o.Bytes)
		result.BulkObservations = append(result.BulkObservations, o)
	}
	addSample := func(kind, store string, started time.Time, n int, e error) {
		mu.Lock()
		defer mu.Unlock()
		status := "PASS"
		if e != nil {
			status = "FAIL"
		}
		if errors.Is(e, context.Canceled) {
			status = "CANCELLED"
		}
		result.Samples = append(result.Samples, Sample{kind, store, time.Since(started).Seconds(), status, n})
	}
	checkHealth := func() error {
		h, e := health(c)
		mu.Lock()
		defer mu.Unlock()
		if e == nil {
			result.Health = append(result.Health, h)
		} else if result.HealthFailure == "" {
			// health() returns bounded diagnostic text, never observer values or
			// paths. Retain the first failed gate after peers have been joined.
			result.HealthFailure = e.Error()
		}
		return e
	}
	if err = checkHealth(); err != nil {
		return result, err
	}
	var application *appProbe
	if round.Mixed {
		application, err = newAppProbe(*c.Performance.Application)
		if err != nil {
			return result, err
		}
		defer application.close()
	}
	sessions := make([]*session, len(round.Stores))
	expected := make([]map[string]Document, len(round.Stores))
	ordered := make([][]Document, len(round.Stores))
	for i, s := range round.Stores {
		sessions[i], err = openSession(ctx, c, s, budget, observer)
		if err != nil {
			return result, err
		}
		clients = append(clients, sessions[i].client)
		// Every round starts with newly authorized absent physical targets. This
		// runner has no delete/reset/reindex path. The formal adapter may remove
		// its own just-created orphan on alias failure; the profile must explicitly
		// authorize that narrow cleanup as well as creation before any connection.
		for _, target := range []string{s.Alias, s.Physical, s.keywordIndex()} {
			status, _ := sessions[i].json(ctx, "HEAD", "/"+target, nil, nil)
			if status != 404 {
				return result, errors.New("performance target already exists or cannot be proven absent")
			}
		}
		expected[i] = map[string]Document{}
		for n := 0; n < round.DocumentsPerStore; n++ {
			if n%32 == 0 {
				if err = checkHealth(); err != nil {
					return result, err
				}
				if err = ctx.Err(); err != nil {
					return result, err
				}
			}
			d := synthetic(s, c.Performance.Seed, n)
			ordered[i] = append(ordered[i], d)
			expected[i][d.ChunkID] = d
		}
		result.Manifests[s.StoreID] = manifest(expected[i])
	}
	if err = checkHealth(); err != nil {
		return result, err
	}
	// A real Save per store establishes a frozen query anchor. It is measured
	// separately from steady-state BatchSave wall time and never hidden warmup.
	for i, s := range round.Stores {
		if err = checkHealth(); err != nil {
			return result, err
		}
		started := time.Now()
		infos, params := parameters(ordered[i][:1])
		err = sessions[i].repo.Save(ctx, infos[0], params)
		addSample("anchor_save", s.StoreID, started, 1, err)
		if err != nil {
			return result, errors.New("anchor write failed; outcome may be unknown")
		}
		if err = sessions[i].alias(ctx, s); err != nil {
			return result, err
		}
		if err = sessions[i].settings(ctx, s); err != nil {
			return result, err
		}
		err = adapter.WaitForVisibility(ctx, time.Duration(c.VisibilitySeconds)*time.Second, time.Duration(c.PollMilliseconds)*time.Millisecond, func(x context.Context) (bool, error) {
			docs, e := sessions[i].scan(x, s, s.Alias, s.Dimension, false, false)
			if e != nil {
				return false, e
			}
			if len(docs) == 0 {
				return false, nil
			}
			return true, reconcile(map[string]Document{ordered[i][0].ChunkID: ordered[i][0]}, docs)
		})
		if err != nil {
			return result, err
		}
	}
	// Pure-index probes exercise formal Retrieve. The hybrid lane invokes the
	// existing application RRF API; no fusion implementation is duplicated here.
	var queryWG sync.WaitGroup
	queryErrors := make(chan error, 1)
	queryCount := 0
	started := time.Now()
	jobs := []job{}
	for start := 1; start < round.DocumentsPerStore; start += round.BatchSize {
		end := min(start+round.BatchSize, round.DocumentsPerStore)
		for i := range sessions {
			jobs = append(jobs, job{store: i, docs: ordered[i][start:end]})
		}
	}
	for i := range jobs {
		jobs[i].at = started
		if round.Mixed {
			jobs[i].at = started.Add(time.Duration(round.MinimumSeconds) * time.Second * time.Duration(i+1) / time.Duration(len(jobs)))
		}
	}
	queryCtx, stopQueries := context.WithCancel(ctx)
	if round.Mixed {
		queryWG.Add(1)
		go func() {
			defer queryWG.Done()
			for queryCtx.Err() == nil {
				if e := checkHealth(); e != nil {
					queryErrors <- e
					cancel()
					return
				}
				for i, s := range round.Stores {
					for _, mode := range []types.RetrieverType{types.KeywordsRetrieverType, types.VectorRetrieverType} {
						if e := checkHealth(); e != nil {
							queryErrors <- e
							cancel()
							return
						}
						p := types.RetrieveParams{Query: "OS02PERF", Embedding: ordered[i][0].Embedding, KnowledgeBaseIDs: []string{s.KnowledgeBaseID}, KnowledgeIDs: []string{s.KnowledgeID}, TopK: 5, RetrieverType: mode, AdditionalParams: map[string]any{"dim": s.Dimension}}
						t0 := time.Now()
						hits, e := sessions[i].repo.Retrieve(queryCtx, p)
						if e == nil {
							e = verifyRetrieval(hits, s, expected[i])
						}
						if e != nil && queryCtx.Err() != nil {
							e = context.Canceled
						}
						addSample("index_"+string(mode), s.StoreID, t0, 0, e)
						if e != nil {
							if queryCtx.Err() == nil {
								queryErrors <- e
								cancel()
							}
							return
						}
					}
				}
				for _, q := range fixedQueries[:3] {
					if e := checkHealth(); e != nil {
						queryErrors <- e
						cancel()
						return
					}
					t0 := time.Now()
					receipt, e := application.run(queryCtx, q)
					if e != nil && queryCtx.Err() != nil {
						e = context.Canceled
					}
					mu.Lock()
					result.ApplicationEvidence = append(result.ApplicationEvidence, receipt)
					mu.Unlock()
					addSample(q.ID, "application", t0, 0, e)
					if e != nil {
						if queryCtx.Err() == nil {
							queryErrors <- e
							cancel()
						}
						return
					}
					queryCount++
				}
				if sleepContext(queryCtx, time.Duration(round.QueryIntervalMilliseconds)*time.Millisecond) != nil {
					return
				}
			}
		}()
	}
	err = dispatch(ctx, cancel, round.Concurrency, jobs, func(x context.Context, j job) error {
		if e := checkHealth(); e != nil {
			return e
		}
		infos, params := parameters(j.docs)
		t0 := time.Now()
		e := sessions[j.store].repo.BatchSave(x, infos, params)
		addSample("batch", round.Stores[j.store].StoreID, t0, len(j.docs), e)
		return e
	})
	result.WallSeconds = time.Since(started).Seconds()
	stopQueries()
	queryWG.Wait()
	select {
	case e := <-queryErrors:
		if err == nil {
			err = e
		}
	default:
	}
	if err != nil {
		if result.HealthFailure != "" {
			return result, fmt.Errorf("load stopped on health gate: %s; in-flight writes are not rolled back", result.HealthFailure)
		}
		return result, errors.New("load stopped on first failure; in-flight writes are not rolled back")
	}
	if round.Mixed && (queryCount < round.MinimumQueries || result.WallSeconds < float64(round.MinimumSeconds)) {
		return result, errors.New("frozen mixed duration/query minimum not achieved")
	}
	visibilityStarted := time.Now()
	for i, s := range round.Stores {
		if err = sessions[i].alias(ctx, s); err != nil {
			return result, err
		}
		var actual map[string]Document
		err = adapter.WaitForVisibility(ctx, time.Duration(c.VisibilitySeconds)*time.Second, time.Duration(c.PollMilliseconds)*time.Millisecond, func(x context.Context) (bool, error) {
			actual, err = sessions[i].scan(x, s, s.Alias, s.Dimension, false, false)
			if err != nil {
				return false, err
			}
			for id, d := range actual {
				e, ok := expected[i][id]
				if !ok || d.hash() != e.hash() {
					return false, errors.New("unexpected record or data corruption")
				}
			}
			return len(actual) == len(expected[i]), nil
		})
		if err != nil {
			return result, err
		}
		if err = reconcile(expected[i], actual); err != nil {
			return result, err
		}
		result.ActualManifests[s.StoreID] = manifest(actual)
		result.CorrectUnique += len(actual) - 1 // anchor Save is reported separately
		if s.Dimension > 0 {
			copies, e := sessions[i].scan(ctx, s, s.keywordIndex(), 0, false, true)
			if e != nil {
				return result, e
			}
			if len(copies) != 0 {
				return result, errors.New("unintended keyword copies")
			}
		}
	}
	result.VisibilitySeconds = time.Since(visibilityStarted).Seconds()
	if err = checkHealth(); err != nil {
		return result, err
	}
	final := budget.Snapshot()
	if final.InFlight != 0 || final.Queued != 0 || final.Writing != 0 {
		return result, errors.New("shared budget not drained")
	}
	result.CorrectUniquePerSecond = float64(result.CorrectUnique) / result.WallSeconds
	result.Status = "PASS"
	return result, nil
}
