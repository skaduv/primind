package nebula

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// Real-service mechanism checks (NG-01B P1/P3/P4/P6 on a live graphd). They run
// only when NEBULA_IT_ADDRS is set and NEBULA_IT_SPACE names an existing
// dedicated primind_ng_* Space; every test writes under its own run prefix and
// deletes what it wrote. NEBULA_IT_KILL_SPACE (another primind_ng_* Space) enables
// the killed-session recovery check, which kills sessions bound to NEBULA_IT_SPACE.
func localRealConfig(t *testing.T) Config {
	t.Helper()
	addrs := os.Getenv("NEBULA_IT_ADDRS")
	if addrs == "" {
		t.Skip("NEBULA_IT_ADDRS not set; real NebulaGraph checks skipped")
	}
	space := os.Getenv("NEBULA_IT_SPACE")
	if !strings.HasPrefix(space, "primind_ng_") {
		t.Fatal("NEBULA_IT_SPACE must be a dedicated primind_ng_* Space")
	}
	cfg := DefaultConfig()
	cfg.Addresses = strings.Split(addrs, ",")
	cfg.Space = space
	cfg.Username = os.Getenv("NEBULA_IT_USERNAME")
	cfg.Password = os.Getenv("NEBULA_IT_PASSWORD")
	return cfg
}

func openLocalReal(t *testing.T, cfg Config) *Repository {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Creates the schema only in an empty Space; otherwise NewRepository validates it.
	if err := InitializeSchema(ctx, cfg, SchemaAuthorization{
		Space:                       cfg.Space,
		CreateInEmptyDedicatedSpace: true,
	}); err != nil && !errors.Is(err, ErrSchema) {
		t.Fatal(err)
	}
	r, err := NewRepository(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return r
}

func runPrefix(t *testing.T) string {
	return fmt.Sprintf("it%d_%s", time.Now().UnixNano()%1e9, strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")))
}

func syntheticGraph(prefix string, nodes, titleLen int) *types.GraphData {
	g := &types.GraphData{}
	pad := strings.Repeat("长", titleLen)
	for i := 0; i < nodes; i++ {
		g.Node = append(g.Node, &types.GraphNode{
			Name:       fmt.Sprintf("%s实体%04d", prefix, i),
			Chunks:     []string{fmt.Sprintf("%s-chunk-%d", prefix, i%7)},
			Attributes: []string{pad},
		})
		if i > 0 {
			g.Relation = append(g.Relation, &types.GraphRelation{
				Node1: g.Node[i-1].Name, Node2: g.Node[i].Name, Type: "关联",
			})
		}
	}
	return g
}

func cleanupNS(t *testing.T, r *Repository, ns types.NameSpace) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := r.DelGraph(ctx, []types.NameSpace{ns}); err != nil {
		t.Errorf("cleanup DelGraph: %v", err)
		return
	}
	g, err := r.Snapshot(ctx, ns)
	if err != nil {
		t.Errorf("cleanup snapshot: %v", err)
	} else if len(g.Node)+len(g.Relation) != 0 {
		t.Errorf("residue after cleanup: %d nodes %d relations", len(g.Node), len(g.Relation))
	}
}

// P1: every executed write statement respects both the record and the byte
// limit, and the split write reads back completely.
func TestLocalRealBatchDualLimits(t *testing.T) {
	cfg := localRealConfig(t)
	cfg.BatchRecords = 7
	cfg.StatementBytes = 4096
	cfg.PendingBytes = 1 << 20
	r := openLocalReal(t, cfg)
	prefix := runPrefix(t)
	ns := types.NameSpace{KnowledgeBase: prefix + "-kb", Knowledge: prefix + "-doc"}
	t.Cleanup(func() { cleanupNS(t, r, ns) })
	_ = r.TakeSamples()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	g := syntheticGraph(prefix, 60, 120)
	if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
		t.Fatal(err)
	}
	writes := 0
	for _, m := range r.TakeSamples() {
		if m.Kind == "schema" || m.Kind == "search" || m.Kind == "snapshot" {
			continue
		}
		writes++
		if m.Records > cfg.BatchRecords || m.Bytes > cfg.StatementBytes {
			t.Fatalf("batch %s exceeds limits: records=%d bytes=%d", m.Kind, m.Records, m.Bytes)
		}
	}
	if writes < 60/cfg.BatchRecords {
		t.Fatalf("expected the write to be split, got %d statements", writes)
	}
	snap, err := r.Snapshot(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Node) != 60 || len(snap.Relation) != 59 {
		t.Fatalf("readback: %d nodes %d relations, want 60/59", len(snap.Node), len(snap.Relation))
	}
	t.Logf("P1: %d write statements, limits records<=%d bytes<=%d", writes, cfg.BatchRecords, cfg.StatementBytes)
}

// P3/P6: concurrent AddGraph calls share one write permit and a bounded wait
// queue; overflow is an explicit ErrCapacity, never a silent drop.
func TestLocalRealSharedAdmission(t *testing.T) {
	cfg := localRealConfig(t)
	cfg.Connections, cfg.Writes, cfg.Waiters = 3, 1, 2
	cfg.BatchRecords = 10
	r := openLocalReal(t, cfg)
	prefix := runPrefix(t)
	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	nss := make([]types.NameSpace, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		nss[i] = types.NameSpace{KnowledgeBase: prefix + "-kb", Knowledge: fmt.Sprintf("%s-doc%d", prefix, i)}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			errs[i] = r.AddGraph(ctx, nss[i], []*types.GraphData{syntheticGraph(fmt.Sprintf("%s%d", prefix, i), 80,
				20)})
		}(i)
	}
	t.Cleanup(func() {
		for _, ns := range nss {
			cleanupNS(t, r, ns)
		}
	})
	close(start)
	wg.Wait()
	ok, capacity := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrCapacity):
			capacity++
		default:
			t.Fatalf("caller %d: unexpected error %v", i, err)
		}
	}
	m := r.Metrics()
	if m.PeakWrites > cfg.Writes || m.Peak > cfg.Connections {
		t.Fatalf("permits exceeded: peak=%d peakWrites=%d", m.Peak, m.PeakWrites)
	}
	if ok == 0 || m.Active != 0 || m.Waiting != 0 {
		t.Fatalf("ok=%d active=%d waiting=%d", ok, m.Active, m.Waiting)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i, err := range errs {
		if err != nil {
			continue
		}
		snap, serr := r.Snapshot(ctx, nss[i])
		if serr != nil || len(snap.Node) != 80 {
			t.Fatalf("caller %d readback: %v nodes=%d", i, serr, len(snap.Node))
		}
	}
	t.Logf("P3: %d succeeded, %d ErrCapacity, peak=%d peakWrites=%d", ok, capacity, m.Peak, m.PeakWrites)
}

// P4: a deadline stops dispatching later batches; nothing keeps running after
// AddGraph returns and permits are released.
func TestLocalRealCancellationStopsLaterBatches(t *testing.T) {
	cfg := localRealConfig(t)
	cfg.BatchRecords = 5
	r := openLocalReal(t, cfg)
	prefix := runPrefix(t)
	ns := types.NameSpace{KnowledgeBase: prefix + "-kb", Knowledge: prefix + "-doc"}
	t.Cleanup(func() { cleanupNS(t, r, ns) })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := r.AddGraph(ctx, ns, []*types.GraphData{syntheticGraph(prefix, 1500, 10)})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("AddGraph after deadline = %v, want context error", err)
	}
	before := r.Metrics()
	time.Sleep(500 * time.Millisecond)
	after := r.Metrics()
	if after.StatementCalls != before.StatementCalls || after.Active != 0 || after.ActiveWrites != 0 ||
		after.PlanReservedBytes != 0 {
		t.Fatalf("work continued after return: calls %d->%d active=%d writes=%d plan=%d",
			before.StatementCalls, after.StatementCalls, after.Active, after.ActiveWrites, after.PlanReservedBytes)
	}
	t.Logf("P4: stopped after %d statements, error=%v", after.StatementCalls, err)
}

// Killed graphd sessions surface as errors (writes are never replayed by the
// adapter); the broken session is discarded and the next call succeeds.
func TestLocalRealKilledSessionRecovery(t *testing.T) {
	cfg := localRealConfig(t)
	killSpace := os.Getenv("NEBULA_IT_KILL_SPACE")
	if !strings.HasPrefix(killSpace, "primind_ng_") || killSpace == cfg.Space {
		t.Skip("NEBULA_IT_KILL_SPACE (a different primind_ng_* Space) not set")
	}
	cfg.Connections, cfg.Writes = 2, 1
	killerCfg := cfg
	killerCfg.Space = killSpace
	killer := openLocalReal(t, killerCfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	listSessions := func() map[string]bool {
		t.Helper()
		table, err := killer.read(ctx, "diag", "SHOW SESSIONS | YIELD $-.SessionId AS SessionId;")
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, row := range table.Rows {
			if id, ok := cell(row, fieldIndex(table, "SessionId")); ok {
				ids[id] = true
			}
		}
		return ids
	}
	// SHOW SESSIONS does not reliably report the bound Space, so the victim's
	// sessions are the ones that appear after the killer is already connected.
	// Two sequential reads fill both killer pool slots before the baseline.
	listSessions()
	listSessions()
	before := listSessions()
	r := openLocalReal(t, cfg)
	prefix := runPrefix(t)
	ns := types.NameSpace{KnowledgeBase: prefix + "-kb", Knowledge: prefix + "-doc"}
	t.Cleanup(func() { cleanupNS(t, r, ns) })
	g := syntheticGraph(prefix, 10, 5)
	if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
		t.Fatal(err)
	}

	killed := 0
	for id := range listSessions() {
		if before[id] {
			continue
		}
		if _, err := killer.read(ctx, "diag", "KILL SESSION "+id+";"); err != nil {
			t.Fatalf("KILL SESSION %s: %v", id, err)
		}
		killed++
	}
	if killed == 0 {
		t.Fatal("no new session of the victim repository was found to kill")
	}

	// First calls on killed sessions must fail visibly, then recover.
	var firstErr error
	recovered := false
	for i := 0; i < 2*cfg.Connections+1 && !recovered; i++ {
		_, err := r.SearchNode(ctx, ns, []string{g.Node[0].Name})
		if err == nil {
			recovered = true
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if !recovered {
		t.Fatalf("no recovery after killing %d sessions: %v", killed, firstErr)
	}
	// Caller-level replay of the same AddGraph is idempotent.
	if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
		t.Fatalf("AddGraph after recovery: %v", err)
	}
	snap, err := r.Snapshot(ctx, ns)
	if err != nil || len(snap.Node) != 10 || len(snap.Relation) != 9 {
		t.Fatalf("readback after recovery: %v %d/%d", err, len(snap.Node), len(snap.Relation))
	}
	t.Logf("session recovery: killed=%d first error=%v", killed, firstErr)
}
