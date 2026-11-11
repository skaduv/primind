package nebula_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/nebula"
	"github.com/primind/primind/internal/types"
)

// The synthetic oracle is literal business data. It does not call adapter
// mapping, graph traversal, AddGraph, or inspect emitted nGQL to derive answers.
func syntheticCalls(run string, count, pairs int, seed int64) ([]adapter.GraphCall, []adapter.GraphCall,
	map[types.NameSpace]fixtureState,
) {
	calls := []adapter.GraphCall{}
	hot := []adapter.GraphCall{}
	expected := map[types.NameSpace]fixtureState{}
	for i := 0; i < count; i++ {
		ns := types.NameSpace{KnowledgeBase: run + "-kb", Knowledge: fmt.Sprintf("%s-doc-%06d", run, i)}
		graph := &types.GraphData{}
		state := fixtureState{
			Entities: []entityKey{}, Nodes: []fixtureNode{}, Relations: []relationKey{},
			Connected: 2 * pairs,
		}
		for pair := 0; pair < pairs; pair++ {
			nameA, nameB := specialName, "调节阀B"
			if pair > 0 {
				nameA += fmt.Sprintf("-%03d", pair)
				nameB += fmt.Sprintf("-%03d", pair)
			}
			chunks := []string{fmt.Sprintf("%s-c-%06d-%03d-1", run, i, pair), fmt.Sprintf("%s-c-%06d-%03d-2", run, i,
				pair)}
			attr := strings.Repeat("参数", 1+int((int64(i+pair)+seed)%16))
			graph.Node = append(graph.Node, &types.GraphNode{Name: nameA, Chunks: chunks, Attributes: []string{attr}},
				&types.GraphNode{Name: nameB, Chunks: chunks[:1], Attributes: []string{}})
			graph.Relation = append(graph.Relation, &types.GraphRelation{Node1: nameA, Node2: nameB, Type: "连接"},
				&types.GraphRelation{Node1: nameA, Node2: nameB, Type: specialType}, &types.GraphRelation{
					Node1: nameB,
					Node2: nameA, Type: "反馈",
				}, &types.GraphRelation{Node1: nameA, Node2: nameA, Type: "自检"})
			keyA, keyB := entityKey{ns.KnowledgeBase, ns.Knowledge, nameA}, entityKey{
				ns.KnowledgeBase, ns.Knowledge,
				nameB,
			}
			state.Entities = append(state.Entities, keyA, keyB)
			state.Nodes = append(state.Nodes, fixtureNode{keyA, chunks, []string{attr}}, fixtureNode{
				keyB, chunks[:1],
				[]string{},
			})
			state.Relations = append(state.Relations, relationKey{ns.KnowledgeBase, ns.Knowledge, nameA, nameB, "连接"},
				relationKey{ns.KnowledgeBase, ns.Knowledge, nameA, nameB, specialType}, relationKey{
					ns.KnowledgeBase,
					ns.Knowledge, nameB, nameA, "反馈",
				}, relationKey{ns.KnowledgeBase, ns.Knowledge, nameA, nameA, "自检"})
		}
		calls = append(calls, adapter.GraphCall{Namespace: ns, Graphs: []*types.GraphData{graph}})
		expected[ns] = state
	}
	hotspot := calls[0].Namespace
	for i := 0; i < count; i++ {
		chunk := fmt.Sprintf("%s-hot-%06d", run, i)
		hot = append(hot, adapter.GraphCall{
			Namespace: hotspot,
			Graphs: []*types.GraphData{{Node: []*types.GraphNode{{
				Name: specialName, Chunks: []string{chunk},
				Attributes: []string{"must-not-overwrite-on-create"},
			}}}},
		})
	}
	return calls, hot, expected
}

type measuredRound struct {
	Run, Mode                                      string
	Calls, Errors                                  int
	WallNS, VisibilityNS                           int64
	LatenciesNS                                    []int64
	ReadLatenciesNS                                []int64
	Metrics                                        adapter.Metrics
	ResourceSamples                                []adapter.ResourceSample
	UniqueEntities, UniqueRelations, UniqueSources int
	NewEntities, NewRelations, NewSources          int
	Reconciled, Cleaned                            bool
	FailureCause                                   string `json:"failure_cause,omitempty"`
}

func sampleResource(p adapter.RunProfile) (adapter.ResourceSample, error) {
	var s adapter.ResourceSample
	fields := map[string]json.RawMessage{}
	if err := readJSONFile(p.ResourceSamplePath, &fields); err != nil {
		return s, err
	}
	for _, name := range []string{
		"at", "cpu_percent", "memory_bytes", "free_disk_bytes", "service_latency_ms",
		"service_error_rate", "connections", "sessions", "disk_read_bytes_per_second", "disk_write_bytes_per_second",
	} {
		if len(fields[name]) == 0 || string(fields[name]) == "null" {
			return s, fmt.Errorf("missing resource observation: %s", name)
		}
	}
	data, _ := json.Marshal(fields)
	if err := strictDecode(data, &s); err != nil {
		return s, err
	}
	return s, p.CheckResource(s, time.Now())
}

// These seams exercise runner control flow offline. Real runs continue to use
// the formal Repository and sampleResource; no E2 check is replaced by a fake.
type replayRepository interface {
	AddGraph(context.Context, types.NameSpace, []*types.GraphData) error
	SearchNode(context.Context, types.NameSpace, []string) (*types.GraphData, error)
	Metrics() adapter.Metrics
}

func replay(ctx context.Context, p adapter.RunProfile, r replayRepository, calls []adapter.GraphCall, concurrency int,
	mode string,
) (measuredRound, error) {
	return replayWithSampler(ctx, p, r, calls, concurrency, mode, sampleResource)
}

func replayWithSampler(ctx context.Context, p adapter.RunProfile, r replayRepository, calls []adapter.GraphCall,
	concurrency int, mode string, readSample func(adapter.RunProfile) (adapter.ResourceSample, error)) (measuredRound,
	error,
) {
	result := measuredRound{Mode: mode}
	start := time.Now()
	local, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan adapter.GraphCall)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var first error
	// Sample throughout the window, including a slow in-flight call. During
	// soak, probe already-populated data concurrently with replayed writes.
	stopMonitor, monitorDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-local.Done():
				return
			case <-ticker.C:
			}
			sample, err := readSample(p)
			mu.Lock()
			result.ResourceSamples = append(result.ResourceSamples, sample)
			mu.Unlock()
			if err == nil && mode == "soak" {
				then := time.Now()
				_, err = r.SearchNode(local, calls[0].Namespace, []string{specialName})
				mu.Lock()
				result.ReadLatenciesNS = append(result.ReadLatenciesNS, int64(time.Since(then)))
				mu.Unlock()
			}
			if err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
				return
			}
		}
	}()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for call := range jobs {
				if local.Err() != nil {
					return
				}
				sample, err := readSample(p)
				if err != nil {
					mu.Lock()
					result.ResourceSamples = append(result.ResourceSamples, sample)
					if first == nil {
						first = err
					}
					mu.Unlock()
					cancel()
					return
				}
				then := time.Now()
				err = r.AddGraph(local, call.Namespace, call.Graphs)
				elapsed := time.Since(then)
				mu.Lock()
				result.Calls++
				result.LatenciesNS = append(result.LatenciesNS, int64(elapsed))
				result.ResourceSamples = append(result.ResourceSamples, sample)
				if err != nil {
					result.Errors++
					if first == nil {
						first = err
					}
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
dispatch:
	for _, call := range calls {
		select {
		case <-local.Done():
			break dispatch
		case jobs <- call:
		}
	}
	close(jobs)
	wg.Wait()
	close(stopMonitor)
	<-monitorDone
	result.WallNS = int64(time.Since(start))
	result.Metrics = r.Metrics()
	if first == nil && local.Err() != nil {
		first = local.Err()
	}
	return result, first
}

func reconcileSynthetic(ctx context.Context, cfg adapter.Config, r *adapter.Repository,
	expected map[types.NameSpace]fixtureState,
) error {
	for ns, want := range expected {
		if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
			g, err := r.Snapshot(ctx, ns)
			if err != nil {
				return err
			}
			state, err := graphState(g, ns.KnowledgeBase)
			if err != nil {
				return err
			}
			return compareState(state, want)
		}); err != nil {
			return err
		}
	}
	return nil
}

func pctl(samples []int64, p float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := max(0, int(math.Ceil(float64(len(sorted))*p))-1)
	return sorted[index]
}

type performanceEvidenceReporter interface {
	Error(...any)
}

// Missing mandatory evidence fails the test without hiding an operation error.
func recordPerformanceEvidence(t performanceEvidenceReporter, path string, value any) {
	if err := writeEvidence(path, value); err != nil {
		t.Error(err)
	}
}

type performanceFailureReporter interface {
	performanceEvidenceReporter
	Fatal(...any)
}

// Keep the primary error and FAIL state even if saving its evidence also fails.
func failPerformanceRound(t performanceFailureReporter, path string, measured measuredRound, checks map[string]any,
	checkID string, primary error,
) {
	// Use the existing public error text; do not unwrap SDK causes or payloads.
	measured.FailureCause = primary.Error()
	checks[checkID] = "FAIL"
	recordPerformanceEvidence(t, path, measured)
	t.Fatal(primary)
}

func recordSoakWindow(t performanceFailureReporter, path string, measured measuredRound, checks map[string]any,
	primary error,
) {
	if primary != nil {
		failPerformanceRound(t, path, measured, checks, "P6", primary)
		return
	}
	if saveErr := writeEvidence(path, measured); saveErr != nil {
		t.Fatal(saveErr)
		return
	}
}

// Only the soak orchestration is injectable. Formal runs still use replay,
// the repository, and time.Now; offline tests never initialize a repository.
type soakHooks struct {
	Now         func() time.Time
	Metrics     func() adapter.Metrics
	TakeSamples func()
	Replay      func(context.Context) (measuredRound, error)
	Probe       func(context.Context) error
	Reconcile   func(context.Context) error
}

type soakEvidence struct {
	Format                 string             `json:"format"`
	Run                    string             `json:"run_id"`
	Status                 string             `json:"status"`
	FailureCause           string             `json:"failure_cause,omitempty"`
	LoadStartedAt          time.Time          `json:"load_started_at"`
	LoadEndedAt            time.Time          `json:"load_ended_at"`
	LoadDurationNS         int64              `json:"load_duration_ns"`
	ReconcileStartedAt     *time.Time         `json:"reconcile_started_at"`
	ReconcileEndedAt       *time.Time         `json:"reconcile_ended_at"`
	ReconcileDurationNS    *int64             `json:"reconcile_duration_ns"`
	ReconcileStatus        string             `json:"reconcile_status"`
	DurationNS             int64              `json:"duration_ns"`
	Calls                  int                `json:"calls"`
	ReadProbes             int                `json:"concurrent_read_probes"`
	SuccessfulWriteBatches int                `json:"successful_write_batches"`
	Before                 adapter.Metrics    `json:"before"`
	Metrics                adapter.Metrics    `json:"metrics"`
	CounterDelta           map[string]*uint64 `json:"counter_delta"`
	Windows                []string           `json:"windows"`
}

type soakWindowEvidence struct {
	measuredRound
	RunID                  string             `json:"run_id"`
	Window                 int                `json:"window"`
	LoadStartedAt          time.Time          `json:"load_started_at"`
	LoadEndedAt            time.Time          `json:"load_ended_at"`
	LoadDurationNS         int64              `json:"load_duration_ns"`
	Before                 adapter.Metrics    `json:"before"`
	CounterDelta           map[string]*uint64 `json:"counter_delta"`
	SuccessfulWriteBatches int                `json:"successful_write_batches"`
}

// Counters are cumulative snapshots. A rollback is retained as null, never an
// unsigned wraparound or a fabricated zero. Reconnects alone are not failures.
func soakCounterDelta(before, after adapter.Metrics) (map[string]*uint64, error) {
	pairs := map[string][2]uint64{
		"statement_calls": {before.StatementCalls, after.StatementCalls},
		"errors":          {before.Errors, after.Errors},
		"sdk_reconnects":  {before.SDKReconnects, after.SDKReconnects},
		"dropped_samples": {before.DroppedSamples, after.DroppedSamples},
	}
	delta := map[string]*uint64{}
	var issues []error
	for _, key := range []string{"statement_calls", "errors", "sdk_reconnects", "dropped_samples"} {
		pair := pairs[key]
		if pair[1] < pair[0] {
			delta[key] = nil
			issues = append(issues, fmt.Errorf("soak counter moved backwards: %s", key))
		} else {
			n := pair[1] - pair[0]
			delta[key] = &n
		}
	}
	if before.Errors != 0 || after.Errors != 0 {
		issues = append(issues, fmt.Errorf("soak retained execution failures"))
	}
	if before.DroppedSamples != 0 || after.DroppedSamples != 0 {
		issues = append(issues, fmt.Errorf("soak retained dropped samples"))
	}
	return delta, errors.Join(issues...)
}

func soakSuccessfulWrites(samples []adapter.Metric) (int, error) {
	writes := 0
	var issue error
	for _, sample := range samples {
		if sample.Failed || sample.ErrorCode != 0 {
			issue = fmt.Errorf("soak retained failed execution sample")
			continue
		}
		switch sample.Kind {
		case "primind_entity", "primind_relation", "primind_source", "primind_from", "primind_to", "primind_owner":
			if sample.Records <= 0 {
				issue = fmt.Errorf("soak write sample has no records")
				continue
			}
			writes++
		}
	}
	return writes, issue
}

func runSoak(ctx context.Context, p adapter.RunProfile, run, dir string, h soakHooks) (primary error) {
	e := soakEvidence{Run: run, LoadStartedAt: h.Now()}
	h.TakeSamples()
	e.Before = h.Metrics()
	defer func() {
		// Freeze load time/counters on every return, before final reconciliation.
		e.LoadEndedAt = h.Now()
		e.Metrics = h.Metrics()
		primary = finishSoak(ctx, p, dir, &e, h, primary)
	}()
	for h.Now().Sub(e.LoadStartedAt) < time.Duration(p.SoakSeconds)*time.Second {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Calls+p.Calls > p.MaxSoakCalls {
			return fmt.Errorf("%w: soak attempt budget reached before frozen duration", adapter.ErrBudget)
		}
		h.TakeSamples()
		before := h.Metrics()
		started := h.Now()
		measured, replayErr := h.Replay(ctx)
		ended := h.Now()
		e.Calls += measured.Calls
		e.ReadProbes += len(measured.ReadLatenciesNS)
		window := soakWindowEvidence{
			measuredRound: measured, RunID: run, Window: len(e.Windows) + 1,
			LoadStartedAt: started, LoadEndedAt: ended, LoadDurationNS: int64(ended.Sub(started)), Before: before,
		}
		var counterErr, sampleErr error
		window.CounterDelta, counterErr = soakCounterDelta(before, measured.Metrics)
		window.SuccessfulWriteBatches, sampleErr = soakSuccessfulWrites(measured.Metrics.Samples)
		e.SuccessfulWriteBatches += window.SuccessfulWriteBatches
		windowErr := errors.Join(replayErr, counterErr, sampleErr)
		if measured.Errors != 0 || measured.Calls != p.Calls || window.LoadDurationNS < 0 {
			windowErr = errors.Join(windowErr, fmt.Errorf("soak incomplete/failed window"))
		}
		if windowErr != nil {
			window.FailureCause = windowErr.Error()
		}
		name := fmt.Sprintf("soak-window-%08d.json", e.Calls)
		e.Windows = append(e.Windows, name)
		saveErr := writeEvidence(filepath.Join(dir, name), window)
		if windowErr != nil || saveErr != nil {
			return errors.Join(windowErr, saveErr)
		}
		if err := h.Probe(ctx); err != nil {
			return err
		}
	}
	return nil
}

func finishSoak(ctx context.Context, p adapter.RunProfile, dir string, e *soakEvidence, h soakHooks,
	primary error,
) error {
	e.Format, e.Status, e.ReconcileStatus = "NG_SOAK_EVIDENCE_V2", "FAIL", "NOT_RUN"
	e.LoadDurationNS = int64(e.LoadEndedAt.Sub(e.LoadStartedAt))
	after := e.Metrics
	var counterErr error
	e.CounterDelta, counterErr = soakCounterDelta(e.Before, after)
	primary = errors.Join(primary, ctx.Err(), counterErr)
	if e.LoadDurationNS < int64(time.Duration(p.SoakSeconds)*time.Second) || e.Calls < p.MinCalls ||
		e.Calls > p.MaxSoakCalls || e.ReadProbes == 0 || e.SuccessfulWriteBatches < p.MinBatches || after.Active != 0 ||
		after.ActiveWrites != 0 || after.Waiting != 0 || after.PlanReservedBytes != 0 || after.PlanWaiters != 0 {
		primary = errors.Join(primary, fmt.Errorf("soak duration/write minimum/calls/read/drain criteria"))
	}
	if primary == nil {
		started := h.Now()
		e.ReconcileStartedAt = &started
		primary = h.Reconcile(ctx)
		ended := h.Now()
		e.ReconcileEndedAt = &ended
		elapsed := int64(ended.Sub(started))
		e.ReconcileDurationNS = &elapsed
		e.ReconcileStatus = "PASS"
		if primary != nil {
			e.ReconcileStatus = "FAIL"
		}
	}
	// Preserve the legacy total duration (including final reconciliation).
	e.DurationNS = int64(h.Now().Sub(e.LoadStartedAt))
	if primary == nil {
		e.Status = "MEASURED"
	} else {
		e.FailureCause = primary.Error()
	}
	return errors.Join(primary, writeEvidence(filepath.Join(dir, "soak.json"), e))
}

func TestNebulaPerformance(t *testing.T) {
	if os.Getenv("PRIMIND_NG_PERF_ENABLE") != "authorized-load-profile" {
		t.Skip("BLOCKED/NOT_RUN: real load budget absent; SKIP is not P6/P7 PASS")
	}
	p, cfg, out := loadRun(t, true)
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeEvidence(filepath.Join(out, "performance-profile.json"), p); err != nil {
		t.Fatal(err)
	}
	if _, err := sampleResource(p); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.MaxDurationSeconds)*time.Second)
	defer cancel()
	csvFile, err := os.OpenFile(filepath.Join(out, "performance-results.csv"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = csvFile.Close() }()
	writer := csv.NewWriter(csvFile)
	defer writer.Flush()
	if err := writer.Write(performanceColumns); err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"P6": "NOT_RUN", "P7": "RUNNING", "C1": "requires separate contract evidence",
		"business_performance": p.BusinessTargetStatus,
	}
	t.Cleanup(func() { recordPerformanceEvidence(t, filepath.Join(out, "performance-checks.json"), checks) })
	runNumber := 0
	runOne := func(batch, concurrency, round int, warm, soak bool) {
		runNumber++
		run := fmt.Sprintf("%s_r%03d", p.RunID, runNumber)
		runDir := filepath.Join(out, run)
		if err := os.Mkdir(runDir, 0o700); err != nil {
			t.Fatal(err)
		}
		localCfg := cfg
		localCfg.BatchRecords = batch
		localCfg.Writes = concurrency
		localCfg.SnapshotRecords = 100000
		calls, hot, expected := syntheticCalls(run, p.Calls, p.GroupPairs, p.Seed)
		all := append(append([]adapter.GraphCall{}, calls...), hot...)
		manifest, err := adapter.BuildManifest(localCfg, run, all)
		if err != nil {
			t.Fatal(err)
		}
		if len(manifest.Vertices)+len(manifest.Edges) > p.MaxPhysicalRecords {
			t.Fatal("physical write budget")
		}
		if err := writeEvidence(filepath.Join(runDir, "run-manifest.json"), manifest); err != nil {
			t.Fatal(err)
		}
		// Freeze human-readable independent logical answers before any connection.
		oracle := map[string]fixtureState{}
		for ns, s := range expected {
			oracle[ns.Knowledge] = s
		}
		if err := writeEvidence(filepath.Join(runDir, "expected.json"), oracle); err != nil {
			t.Fatal(err)
		}
		r, err := adapter.NewRepository(ctx, localCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			drain, c := context.WithTimeout(context.Background(), time.Duration(p.DrainSeconds)*time.Second)
			defer c()
			if err := r.Close(drain); err != nil {
				t.Error(err)
			}
		}()
		versions, err := r.InspectService(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeEvidence(filepath.Join(runDir, "versions.json"), versions); err != nil {
			t.Fatal(err)
		}
		residue, err := r.ProbeManifest(ctx, manifest)
		if err != nil || len(residue.Vertices)+len(residue.Edges) > 0 {
			t.Fatalf("non-empty run: %v", err)
		}
		var rows []measuredRound
		modes := []string{"new", "replay", "hotspot"}
		for _, mode := range modes {
			input := calls
			if mode == "hotspot" {
				input = hot
				ns := calls[0].Namespace
				st := expected[ns]
				for _, h := range hot {
					st.Nodes[0].Chunks = append(st.Nodes[0].Chunks, h.Graphs[0].Node[0].Chunks[0])
				}
				expected[ns] = st
			}
			_ = r.TakeSamples()
			before := r.Metrics()
			measured, err := replay(ctx, p, r, input, concurrency, mode)
			measured.Run = run
			if err != nil {
				failPerformanceRound(t, filepath.Join(runDir, mode+"-failed.json"), measured, checks, "P7", err)
			}
			visibility := time.Now()
			if err := reconcileSynthetic(ctx, localCfg, r, expected); err != nil {
				checks["P7"] = "FAIL"
				t.Fatal(err)
			}
			measured.VisibilityNS = int64(time.Since(visibility))
			measured.Reconciled = true
			// Totals and newly created records are separate. Replay creates zero;
			// IF NOT EXISTS attempts are not counted as new database records.
			measured.UniqueEntities = 2 * p.GroupPairs * p.Calls
			measured.UniqueRelations = 4 * p.GroupPairs * p.Calls
			measured.UniqueSources = 3 * p.GroupPairs * p.Calls
			switch mode {
			case "hotspot":
				measured.UniqueSources += p.Calls
				measured.NewSources = p.Calls
			case "new":
				measured.NewEntities = measured.UniqueEntities
				measured.NewRelations = measured.UniqueRelations
				measured.NewSources = measured.UniqueSources
			}
			measured.Metrics.Samples = measured.Metrics.Samples[len(before.Samples):]
			measured.Metrics.StatementCalls -= before.StatementCalls
			measured.Metrics.SDKReconnects -= before.SDKReconnects
			if measured.Metrics.DroppedSamples != 0 {
				t.Fatal("bounded metric buffer filled; incomplete percentile evidence")
			}
			rows = append(rows, measured)
		}
		if soak {
			err := runSoak(ctx, p, run, runDir, soakHooks{
				Now: time.Now, Metrics: r.Metrics,
				TakeSamples: func() { _ = r.TakeSamples() },
				Replay: func(ctx context.Context) (measuredRound, error) {
					return replay(ctx, p, r, calls,
						concurrency, "soak")
				},
				Probe: func(ctx context.Context) error {
					queryCtx, c := context.WithTimeout(ctx, localCfg.ReadyTimeout)
					defer c()
					_, err := r.SearchNode(queryCtx, calls[0].Namespace, []string{specialName})
					return err
				},
				Reconcile: func(ctx context.Context) error { return reconcileSynthetic(ctx, localCfg, r, expected) },
			})
			if err != nil {
				checks["P6"] = "FAIL"
				t.Fatal(err)
			}
			checks["P6"] = "MEASURED; C1 and resource trend review required"
		}
		for _, call := range calls {
			if err := r.DelGraph(ctx, []types.NameSpace{call.Namespace}); err != nil {
				t.Fatal(err)
			}
		}
		if err := waitCheck(ctx, localCfg, func(ctx context.Context) error {
			var e error
			residue, e = r.ProbeManifest(ctx, manifest)
			if e != nil {
				return e
			}
			if len(residue.Vertices)+len(residue.Edges) > 0 {
				return fmt.Errorf("residue")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeEvidence(filepath.Join(runDir, "residue.json"), residue); err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			m := &rows[i]
			m.Cleaned = true
			if err := writeEvidence(filepath.Join(runDir, m.Mode+".json"), m); err != nil {
				t.Fatal(err)
			}
			if warm {
				continue
			}
			batchLatencies := []int64{}
			for _, b := range m.Metrics.Samples {
				batchLatencies = append(batchLatencies, b.WaitNS+b.RPCNS)
			}
			maxCPU := 0.0
			maxReadIO, maxWriteIO := 0.0, 0.0
			var maxMemory uint64
			minDisk := ^uint64(0)
			maxConn, maxSessions := 0, 0
			for _, s := range m.ResourceSamples {
				maxCPU = max(maxCPU, s.CPUPercent)
				maxReadIO = max(maxReadIO, s.DiskReadBytesPerSecond)
				maxWriteIO = max(maxWriteIO, s.DiskWriteBytesPerSecond)
				maxMemory = max(maxMemory, s.MemoryBytes)
				minDisk = min(minDisk, s.FreeDiskBytes)
				maxConn = max(maxConn, s.Connections)
				maxSessions = max(maxSessions, s.Sessions)
			}
			physical := map[string]int{}
			for _, b := range m.Metrics.Samples {
				if !b.Failed {
					physical[b.Kind] += b.Records
				}
			}
			seconds := float64(m.WallNS) / float64(time.Second)
			numbers := []any{
				m.Run, round, m.Mode, batch, concurrency, m.Calls, m.WallNS,
				float64(m.Calls-m.Errors) / seconds, pctl(m.LatenciesNS, .5), pctl(m.LatenciesNS, .95), m.VisibilityNS,
				m.UniqueEntities, m.UniqueRelations, m.UniqueSources, m.NewEntities, m.NewRelations, m.NewSources,
				float64(m.NewEntities) / seconds, float64(m.NewRelations) / seconds, float64(m.NewSources) / seconds,
				physical["primind_entity"], physical["primind_relation"], physical["primind_source"],
				physical["primind_from"] + physical["primind_to"], physical["primind_owner"], m.Metrics.StatementCalls,
				m.Metrics.SDKReconnects, m.Errors, m.Metrics.Peak, m.Metrics.PeakWrites, len(batchLatencies),
				pctl(batchLatencies, .5), pctl(batchLatencies, .95), maxCPU, maxMemory, minDisk, maxReadIO, maxWriteIO,
				maxConn, maxSessions, true, true,
			}
			record := make([]string, len(numbers))
			for i, v := range numbers {
				record[i] = fmt.Sprint(v)
			}
			if err := writer.Write(record); err != nil {
				t.Fatal(err)
			}
			writer.Flush()
			if err := writer.Error(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, batch := range p.BatchSizes {
		for _, concurrency := range p.Concurrency {
			runOne(batch, concurrency, 0, true, false)
			for round := 1; round <= p.Rounds; round++ {
				runOne(batch, concurrency, round, false, false)
			}
		}
	}
	runOne(p.BatchSizes[len(p.BatchSizes)-1], p.Concurrency[len(p.Concurrency)-1], 0, false, true)
	checks["P7"] = "MEASURED; source logs, C1, resource trend and suitability review required"
}

var performanceColumns = []string{
	"run", "round", "mode", "batch_records", "concurrency", "calls", "wall_ns",
	"successful_addgraph_per_second", "call_p50_ns", "call_p95_ns", "visibility_ns", "reconciled_total_entities",
	"reconciled_total_relations", "reconciled_total_sources", "new_entities", "new_relations", "new_sources",
	"new_entities_per_second", "new_relations_per_second", "new_sources_per_second", "confirmed_entity_records",
	"confirmed_relation_records", "confirmed_source_records", "confirmed_relation_link_records",
	"confirmed_owner_records", "statement_calls", "sdk_reconnects", "errors", "peak_inflight", "peak_writes",
	"batch_sample_count", "batch_p50_ns", "batch_p95_ns", "max_cpu_percent", "max_memory_bytes", "min_disk_free_bytes",
	"max_disk_read_bytes_per_second", "max_disk_write_bytes_per_second", "max_connections", "max_sessions",
	"reconciled", "cleaned",
}

func TestPerformanceProfileRejectsUnapprovedOrUnsafeRuns(t *testing.T) {
	p := adapter.RunProfile{}
	if p.Validate(true) == nil {
		t.Fatal("empty profile accepted")
	}
	p.Status = "AUTHORIZED"
	p.AuthorizationRef = "test-only"
	p.AllowWrite = true
	p.AllowCleanup = true
	p.RunID = "test"
	p.Space = "test"
	p.Version = "3.8.0"
	p.SDK = "v3.8.0"
	p.SourceCommit = strings.Repeat("a", 40)
	p.Instances = 1
	p.Connections = 4
	p.C1ConnectionBudget = 12
	p.MaxConcurrency = 2
	p.C1Rounds = 3
	p.MaxDurationSeconds = 3600
	p.DrainSeconds = 30
	p.StatementBytes = 65536
	if err := p.Validate(false); err != nil {
		t.Fatal(err)
	}
	if p.Validate(true) == nil {
		t.Fatal("load without resource budget accepted")
	}
	p.Environment = map[string]string{}
	for _, k := range []string{
		"cpu", "memory_limit", "disk_type", "disk_available", "network", "schema", "index",
		"partitions", "replicas", "background_load", "window", "resource_sampler",
	} {
		p.Environment[k] = "synthetic local test"
	}
	p.Calls = 20
	p.GroupPairs = 1
	p.MaxSoakCalls = 1000
	p.MaxPhysicalRecords = 500
	p.Rounds = 3
	p.SoakSeconds = 10
	p.MinBatches = 10
	p.MinCalls = 10
	p.BatchSizes = []int{50, 200}
	p.Concurrency = []int{1, 2}
	p.ResourceSamplePath = "synthetic.json"
	p.MaxCPUPercent = 60
	p.MaxMemoryBytes = 1 << 30
	p.MaxDiskIOBytesPerSecond = 1 << 30
	p.MinFreeDiskBytes = 1 << 30
	p.MaxServiceLatencyMS = 100
	p.MaxServiceErrorRate = .01
	if err := p.Validate(true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := adapter.ResourceSample{At: now, FreeDiskBytes: 2 << 30}
	if err := p.CheckResource(s, now); err != nil {
		t.Fatal(err)
	}
	s.At = now.Add(-11 * time.Second)
	if p.CheckResource(s, now) == nil {
		t.Fatal("stale resource accepted")
	}
	s.At = now
	s.Connections = 5
	if p.CheckResource(s, now) == nil {
		t.Fatal("connection threshold ignored")
	}
	calls, hot, expected := syntheticCalls("test01", 4, 1, 7)
	if len(calls) != 4 || len(hot) != 4 || len(expected) != 4 {
		t.Fatal("dataset counts")
	}
	for ns, state := range expected {
		if len(state.Entities) != 2 || len(state.Relations) != 4 || len(state.Nodes[0].Chunks) != 2 ||
			!strings.HasPrefix(ns.Knowledge,
				"test01-") {
			t.Fatal("synthetic oracle")
		}
	}
	// Validate the actual per-round run naming and planned physical scope before
	// an authorized run can reach any network operation.
	roundCalls, roundHot, _ := syntheticCalls("test01_r001", 4, 2, 7)
	manifestCfg := adapter.DefaultConfig()
	manifestCfg.Space = "test"
	manifest, err := adapter.BuildManifest(manifestCfg, "test01_r001", append(roundCalls, roundHot...))
	if err != nil || len(manifest.Vertices) != 76 || len(manifest.Edges) != 92 {
		t.Fatalf("planned round physical identities: vertices=%d edges=%d err=%v", len(manifest.Vertices),
			len(manifest.Edges), err)
	}
	// Preserve a reproducible format and avoid accidental credential fields.
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "password") {
		t.Fatal("credential profile")
	}
}
