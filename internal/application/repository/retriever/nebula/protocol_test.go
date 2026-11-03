package nebula

import (
	"context"
	"errors"
	"fmt"
	"github.com/primind/primind/internal/types"
	ng "github.com/vesoft-inc/nebula-go/v3"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type protocolSession struct {
	result          *ng.ResultSet
	err             error
	calls, releases atomic.Int32
}

func (s *protocolSession) Execute(context.Context, string) (*ng.ResultSet, error) {
	s.calls.Add(1)
	return s.result, s.err
}
func (s *protocolSession) Release() { s.releases.Add(1) }
func TestSDKLeaseReturnAndServerFailure(t *testing.T) {
	for _, code := range []nv.ErrorCode{nv.ErrorCode_SUCCEEDED, nv.ErrorCode_E_PARTIAL_SUCCEEDED, nv.ErrorCode_E_BAD_PERMISSION} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			rs, _ := ng.GenResultSet(&gr.ExecutionResponse{ErrorCode: code})
			session := &protocolSession{result: rs}
			d := &sdkDriver{cfg: testConfig(), sessions: make(chan sdkSession, 1)}
			d.sessions <- session
			out := d.Execute(context.Background(), "synthetic")
			if !out.Sent || (code == nv.ErrorCode_SUCCEEDED) != (out.Err == nil) {
				t.Fatal("SDK server status swallowed")
			}
			if len(d.sessions) != 1 || session.calls.Load() != 1 || session.releases.Load() != 0 {
				t.Fatal("lease not returned")
			}
		})
	}
	session := &protocolSession{err: errors.New("transport response lost")}
	d := &sdkDriver{cfg: testConfig(), sessions: make(chan sdkSession, 1)}
	d.sessions <- session
	out := d.Execute(context.Background(), "synthetic")
	if !out.Sent || out.Err == nil || session.releases.Load() != 1 || len(d.sessions) != 1 {
		t.Fatal("broken lease not released")
	}
	if <-d.sessions != nil {
		t.Fatal("broken session reused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out = d.Execute(ctx, "synthetic")
	if out.Sent || !errors.Is(out.Err, context.Canceled) {
		t.Fatal("waiting lease cancellation")
	}
}
func TestRegistryInitializationWaitIsCancelable(t *testing.T) {
	cfg := testConfig()
	reg := &registry{entries: map[[32]byte]*poolEntry{}}
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan *Repository, 1)
	factory := func(context.Context, Config) (wireDriver, error) { close(entered); <-release; return &testWire{}, nil }
	go func() { r, _ := reg.open(context.Background(), cfg, factory); first <- r }()
	waitSignal(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := reg.open(ctx, cfg, factory)
	requireErr(t, err, context.DeadlineExceeded)
	close(release)
	r := <-first
	if r == nil {
		t.Fatal("initialization failed")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestSearchCapsAreVisibleAndRawSeedOrder(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	seedState := goldenState{}
	for i := 0; i < 201; i++ {
		name := fmt.Sprintf("泵%03d", i)
		seedState.Nodes = append(seedState.Nodes, goldenNode{Key: goldenKey{ns.KnowledgeBase, ns.Knowledge, name}})
		seedState.Relations = append(seedState.Relations, goldenRelation{ns.KnowledgeBase, ns.Knowledge, name, name, "自检"})
	}
	r := testRepo(cfg, goldenWire(t, cfg, seedState, ns))
	graph, diag, err := r.Search(context.Background(), ns, []string{"泵", "泵200"})
	if err != nil || len(graph.Relation) != 200 || !diag.SeedTruncated || diag.RelationTruncated || graph.Node[0].Name != "泵200" {
		t.Fatalf("seed cap/order: %v %+v", err, diag)
	}
	if r.Metrics().SearchTruncations != 1 {
		t.Fatal("interface-visible counter missing")
	}
	relationState := goldenState{Nodes: []goldenNode{{Key: goldenKey{ns.KnowledgeBase, ns.Knowledge, "A"}}, {Key: goldenKey{ns.KnowledgeBase, ns.Knowledge, "B"}}}}
	for i := 0; i < 2001; i++ {
		relationState.Relations = append(relationState.Relations, goldenRelation{ns.KnowledgeBase, ns.Knowledge, "A", "B", fmt.Sprintf("type%d", i)})
	}
	r = testRepo(cfg, goldenWire(t, cfg, relationState, ns))
	graph, diag, err = r.Search(context.Background(), ns, []string{"A"})
	if err != nil || len(graph.Relation) != 2000 || diag.SeedTruncated || !diag.RelationTruncated {
		t.Fatalf("edge cap: %v %+v", err, diag)
	}
}
func TestSnapshotRejectsMissingLinksAndForeignNamespace(t *testing.T) {
	cfg := testConfig()
	gold := loadGolden(t)
	ns := testNS()
	for _, corruption := range []string{"edge", "namespace", "identity", "attribute"} {
		t.Run(corruption, func(t *testing.T) {
			wire := goldenWire(t, cfg, gold.Baseline, ns)
			original := wire.fn
			wire.fn = func(ctx context.Context, s string) wireOutcome {
				out := original(ctx, s)
				if corruption == "edge" && strings.HasPrefix(s, "LOOKUP ON primind_from ") {
					out.Table.Rows = nil
				}
				if len(out.Table.Rows) > 0 && strings.HasPrefix(s, "LOOKUP ON primind_entity ") {
					switch corruption {
					case "namespace":
						out.Table.Rows[0][1] = encoded("foreign-kb")
					case "identity":
						out.Table.Rows[0][0] = "ng1_fake"
					case "attribute":
						out.Table.Rows[0][4] = encoded("{")
					}
				}
				return out
			}
			graph, err := testRepo(cfg, wire).Snapshot(context.Background(), ns)
			if err == nil || graph != nil {
				t.Fatal("invalid snapshot returned partial success")
			}
		})
	}
}
func TestOperationDeadlineStopsLaterBatches(t *testing.T) {
	cfg := testConfig()
	cfg.OperationTimeout = 10 * time.Millisecond
	w := &testWire{fn: func(context.Context, string) wireOutcome {
		time.Sleep(20 * time.Millisecond)
		return wireOutcome{Sent: true}
	}}
	err := testRepo(cfg, w).AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph()})
	requireErr(t, err, context.DeadlineExceeded)
	if len(w.stmts) != 1 {
		t.Fatal("later work dispatched after deadline")
	}
}

func schemaWire(cfg Config) *testWire {
	return &testWire{fn: func(_ context.Context, s string) wireOutcome {
		t := wireTable{}
		switch {
		case strings.HasPrefix(s, "DESCRIBE SPACE "):
			t = wireTable{Columns: []string{"Vid Type"}, Rows: [][]string{{fmt.Sprintf("FIXED_STRING(%d)", cfg.VIDBytes)}}}
		case s == "SHOW TAGS;" || s == "SHOW EDGES;":
			t = wireTable{Columns: []string{"ID", "Name"}}
		case strings.HasPrefix(s, "CREATE "):
		case strings.HasPrefix(s, "DESCRIBE TAG "):
			tag := strings.TrimSuffix(strings.TrimPrefix(s, "DESCRIBE TAG "), ";")
			t.Columns = []string{"Field", "Type", "Null", "Default", "Comment"}
			for _, name := range tagFields[tag] {
				t.Rows = append(t.Rows, []string{name, "string", "NO", "", ""})
			}
		case strings.HasPrefix(s, "DESCRIBE EDGE "):
			t.Columns = []string{"Field", "Type", "Null", "Default", "Comment"}
			t.Rows = [][]string{{"kb", "string", "NO", "", ""}, {"doc", "string", "NO", "", ""}}
		case strings.HasPrefix(s, "SHOW CREATE EDGE INDEX "):
			name := strings.TrimSuffix(strings.TrimPrefix(s, "SHOW CREATE EDGE INDEX "), "_scope;")
			t = wireTable{Columns: []string{"Edge Index Name", "Create Edge Index"}, Rows: [][]string{{name + "_scope", "CREATE EDGE INDEX " + name + "_scope ON " + name + "(kb(128),doc(128))"}}}
		case strings.HasPrefix(s, "SHOW CREATE TAG INDEX "):
			tag := strings.TrimSuffix(strings.TrimPrefix(s, "SHOW CREATE TAG INDEX "), "_scope;")
			t = wireTable{Columns: []string{"Tag Index Name", "Create Tag Index"}, Rows: [][]string{{tag + "_scope", "CREATE TAG INDEX " + tag + "_scope ON " + tag + "(kb(128), doc(128))"}}}
		case strings.HasPrefix(s, "SHOW CREATE TAG "):
			t = wireTable{Columns: []string{"Tag", "Create Tag"}, Rows: [][]string{{"tag", "CREATE TAG x() ttl_duration = 0, ttl_col = \"\""}}}
		case strings.HasPrefix(s, "SHOW CREATE EDGE "):
			t = wireTable{Columns: []string{"Edge", "Create Edge"}, Rows: [][]string{{"edge", "CREATE EDGE x() ttl_duration = 0, ttl_col = \"\""}}}
		case strings.HasPrefix(s, "LOOKUP "):
			tag := strings.Split(s, " ")[2]
			t.Columns = append([]string{"vid"}, tagFields[tag]...)
		default:
			return wireOutcome{Err: ErrInvalid}
		}
		return wireOutcome{Sent: true, Table: t}
	}}
}
func TestSchemaWaitAndIncompatibleStructures(t *testing.T) {
	cfg := testConfig()
	cfg.PollInterval = time.Millisecond
	w := schemaWire(cfg)
	respond := w.fn
	delayed := false
	definitionReady := false
	w.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.HasPrefix(s, "DESCRIBE TAG ") {
			if !delayed {
				delayed = true
				return wireOutcome{Sent: true, Err: ErrSchema}
			}
			definitionReady = true
		}
		if strings.HasPrefix(s, "CREATE TAG INDEX ") && !definitionReady {
			t.Error("index sent before tag readiness")
		}
		return respond(ctx, s)
	}
	if err := testRepo(cfg, w).initializeEmptySchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !delayed || !definitionReady {
		t.Fatal("readiness not exercised")
	}
	for _, mutation := range []string{"vid", "existing", "ttl", "index"} {
		t.Run(mutation, func(t *testing.T) {
			w := schemaWire(cfg)
			respond := w.fn
			w.fn = func(ctx context.Context, s string) wireOutcome {
				out := respond(ctx, s)
				switch {
				case mutation == "vid" && strings.HasPrefix(s, "DESCRIBE SPACE "):
					out.Table.Rows[0][0] = "INT64"
				case mutation == "existing" && s == "SHOW TAGS;":
					out.Table.Rows = [][]string{{"1", "foreign"}}
				case mutation == "ttl" && strings.HasPrefix(s, "SHOW CREATE TAG ") && !strings.Contains(s, "INDEX"):
					out.Table.Rows[0][1] = "ttl_duration = 99, ttl_col = \"expiry\""
				case mutation == "index" && strings.HasPrefix(s, "SHOW CREATE TAG INDEX "):
					out.Table.Rows[0][1] = "CREATE TAG INDEX wrong ON other(kb(128),doc(128))"
				}
				return out
			}
			r := testRepo(cfg, w)
			var err error
			if mutation == "existing" {
				err = r.initializeEmptySchema(context.Background())
			} else {
				err = r.CheckSchema(context.Background())
			}
			if err == nil {
				t.Fatal("incompatible schema accepted")
			}
			for _, s := range w.stmts {
				if strings.HasPrefix(s, "CREATE ") {
					t.Fatal("mismatch mutated schema")
				}
			}
		})
	}
}

func TestTransportDeadlineAndCancellationCloseActualConnection(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			local, peer := net.Pipe()
			defer peer.Close()
			defer local.Close()
			bounded := &deadlineConn{Conn: local}
			s := &wireSession{conn: bounded, timeout: 20 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- s.rpcCall(ctx, func() error {
					// An SDK per-read refresh cannot extend this RPC's absolute timeout.
					if err := bounded.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
						return err
					}
					close(entered)
					_, err := bounded.Read(make([]byte, 1))
					return err
				})
			}()
			waitSignal(t, entered)
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked socket falsely succeeded")
				}
				if mode == "cancel" {
					requireErr(t, err, context.Canceled)
				}
			case <-time.After(time.Second):
				t.Fatal("transport not bounded")
			}
		})
	}
}

func TestOrphanInternalEdgesCanBeDeletedWithoutVertices(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	wire := goldenWire(t, cfg, loadGolden(t).Baseline, ns)
	original := wire.fn
	deletes := 0
	wire.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.HasPrefix(s, "DELETE EDGE ") {
			deletes++
			return wireOutcome{Sent: true}
		}
		if strings.HasPrefix(s, "DELETE VERTEX ") {
			t.Error("no vertices should remain")
			return wireOutcome{Sent: true}
		}
		out := original(ctx, s)
		for _, tag := range []string{entityTag, relationTag, sourceTag} {
			if strings.HasPrefix(s, "LOOKUP ON "+tag+" ") {
				out.Table.Rows = nil
			}
		}
		return out
	}
	r := testRepo(cfg, wire)
	if g, err := r.Snapshot(context.Background(), ns); err == nil || g != nil {
		t.Fatal("orphan graph falsely decoded")
	}
	if err := r.DelGraph(context.Background(), []types.NameSpace{ns}); err != nil {
		t.Fatal(err)
	}
	if deletes != 3 {
		t.Fatalf("orphan edge groups deleted: %d", deletes)
	}
}
func TestWholeWritePlanMemoryIsBoundedAndDrainedOnClose(t *testing.T) {
	cfg := testConfig()
	w := &testWire{}
	r := testRepo(cfg, w)
	var releases []func()
	for i := 0; i < cfg.PlanBytes/cfg.OperationBytes; i++ {
		release, err := r.c.reservePlan(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := r.c.reservePlan(ctx)
	requireErr(t, err, context.DeadlineExceeded)
	if r.Metrics().PlanReservedBytes != cfg.PlanBytes {
		t.Fatal("planning budget exceeded")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	requireErr(t, r.Close(ctx2), context.DeadlineExceeded)
	if w.closed.Load() != 0 {
		t.Fatal("closed while planning remained")
	}
	for _, release := range releases {
		release()
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Metrics().PlanReservedBytes != 0 || w.closed.Load() != 1 {
		t.Fatal("planning not drained")
	}
}
func TestFormalSearchAndDeleteShareWriteSaturationBudget(t *testing.T) {
	cfg := testConfig()
	cfg.Connections = 3
	cfg.Writes = 2
	ns := testNS()
	wire := goldenWire(t, cfg, loadGolden(t).Baseline, ns)
	original := wire.fn
	entered, release := make(chan struct{}, 2), make(chan struct{})
	deleteCalls := atomic.Int32{}
	wire.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.HasPrefix(s, "INSERT ") {
			entered <- struct{}{}
			<-release
			return wireOutcome{Sent: true}
		}
		if strings.HasPrefix(s, "DELETE ") {
			deleteCalls.Add(1)
			return wireOutcome{Sent: true}
		}
		return original(ctx, s)
	}
	r := testRepo(cfg, wire)
	writes := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			writes <- r.AddGraph(context.Background(), ns, []*types.GraphData{{Node: []*types.GraphNode{{Name: "one"}}}})
		}()
		waitSignal(t, entered)
	}
	got, err := r.SearchNode(context.Background(), ns, []string{"主泵A"})
	if err != nil || len(got.Relation) != 5 {
		t.Fatalf("reserved search: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	requireErr(t, r.DelGraph(ctx, []types.NameSpace{ns}), context.DeadlineExceeded)
	if deleteCalls.Load() != 0 {
		t.Fatal("delete bypassed write budget")
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-writes; err != nil {
			t.Fatal(err)
		}
	}
	if m := r.Metrics(); m.Peak > 3 || m.PeakWrites > 2 || m.PlanReservedBytes != 0 {
		t.Fatalf("budget/leak: %+v", m)
	}
}
