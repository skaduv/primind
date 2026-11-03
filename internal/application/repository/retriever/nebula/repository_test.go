package nebula

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/primind/primind/internal/types"
	ng "github.com/vesoft-inc/nebula-go/v3"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testWire struct {
	fn     func(context.Context, string) wireOutcome
	mu     sync.Mutex
	stmts  []string
	closed atomic.Int32
}

func (w *testWire) Execute(ctx context.Context, s string) wireOutcome {
	w.mu.Lock()
	w.stmts = append(w.stmts, s)
	w.mu.Unlock()
	if w.fn != nil {
		return w.fn(ctx, s)
	}
	return wireOutcome{Sent: true}
}
func (w *testWire) Close()           { w.closed.Add(1) }
func (*testWire) RetryCount() uint64 { return 0 }
func testConfig() Config {
	c := DefaultConfig()
	c.Addresses = []string{"127.0.0.1:9669"}
	c.Space = "local_protocol_only"
	c.Username = "synthetic"
	c.Password = "synthetic"
	return c
}
func testRepo(c Config, w wireDriver) *Repository { return &Repository{c: newClient(c, w)} }
func testNS() types.NameSpace                     { return types.NameSpace{KnowledgeBase: "kb-a", Knowledge: "doc-a1"} }
func sampleGraph() *types.GraphData {
	return &types.GraphData{Node: []*types.GraphNode{{Name: "泵\"Q\\X\n第二行", Chunks: []string{"c1", "c2"}, Attributes: []string{"a\n\""}}, {Name: "阀"}}, Relation: []*types.GraphRelation{{Node1: "泵\"Q\\X\n第二行", Node2: "阀", Type: "连接"}, {Node1: "泵\"Q\\X\n第二行", Node2: "阀", Type: "反馈"}}}
}
func requireErr(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("want %v, got %v", target, err)
	}
}
func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("coordination timeout")
	}
}
func TestIdentityAndPropertyCodec(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	id, err := identity(cfg, "entity", ns, "主泵A")
	if err != nil {
		t.Fatal(err)
	}
	// 泵 is U+6CF5 (UTF-8 E6 B3 B5), not U+6CF3. Independent tuple vector.
	const known = "ng1_WyJlbnRpdHkiLCJrYi1hIiwiZG9jLWExIiwi5Li75rO1QSJd"
	if id != known {
		t.Fatalf("fixed identity vector: %s", id)
	}
	keys := map[string]bool{}
	for _, n := range []types.NameSpace{ns, {KnowledgeBase: "kb_a", Knowledge: ns.Knowledge}, {KnowledgeBase: "kb-a", Knowledge: "doc-a2"}} {
		for _, role := range []string{"entity", "source", "relation"} {
			x, e := identity(cfg, role, n, "A|B", "C")
			if e != nil || keys[x] {
				t.Fatalf("collision/error %v", e)
			}
			keys[x] = true
		}
	}
	for _, v := range []string{"泵\"Q\\X\n第二行", "\x00\x01\t\r\n", "a'); DROP SPACE x; --", "中文😀", ""} {
		back, err := decoded(encoded(v))
		if err != nil || back != v {
			t.Fatal("codec mismatch")
		}
	}
	if _, err := decoded("bad="); err == nil {
		t.Fatal("noncanonical encoding accepted")
	}
	_, err = identity(cfg, "entity", ns, strings.Repeat("长", 1000))
	requireErr(t, err, ErrBudget)
	requireErr(t, validateNamespace(types.NameSpace{}, false), ErrInvalid)
}
func TestP1BatchLimitsAndDependency(t *testing.T) {
	cfg := testConfig()
	cfg.BatchRecords = 2
	cfg.StatementBytes = 256
	rs := []record{{"a", strings.Repeat("中", 30)}, {"b", strings.Repeat("x", 100)}, {"c", "ok"}}
	bs, err := splitBatches(cfg, "test", "PREFIX ", ";", rs)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 || len(bs[0].IDs) != 2 || len(bs[1].IDs) != 1 {
		t.Fatalf("batch boundary %+v", bs)
	}
	for _, b := range bs {
		if len(b.Statement) > 256 || len(b.IDs) > 2 {
			t.Fatal("budget exceeded")
		}
	}
	_, err = splitBatches(cfg, "test", "PREFIX ", ";", []record{{"big", strings.Repeat("中", 100)}})
	requireErr(t, err, ErrBudget)
	empty, err := splitBatches(cfg, "test", "x", ";", nil)
	if err != nil || len(empty) != 0 {
		t.Fatal("empty sends")
	}
	cfg = testConfig()
	cfg.BatchRecords = 2
	w := &testWire{}
	r := testRepo(cfg, w)
	if err := r.AddGraph(context.Background(), testNS(), nil); err != nil || len(w.stmts) != 0 {
		t.Fatal("empty AddGraph RPC")
	}
	if err := r.AddGraph(context.Background(), testNS(), []*types.GraphData{sampleGraph(), sampleGraph()}); err != nil {
		t.Fatal(err)
	}
	plan, err := r.addPlan(testNS(), []*types.GraphData{sampleGraph()})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.stmts) != len(plan)*2 {
		t.Fatal("multi-GraphData lost")
	}
	seenEntity, seenSource, seenRelation := false, false, false
	for _, b := range plan {
		if len(b.IDs) > 2 || len(b.Statement) > cfg.StatementBytes {
			t.Fatal("plan bounds")
		}
		switch b.Kind {
		case entityTag:
			seenEntity = true
		case sourceTag:
			if !seenEntity {
				t.Fatal("source before entity")
			}
			seenSource = true
		case relationTag:
			if !seenEntity {
				t.Fatal("relation before entity")
			}
			seenRelation = true
		case "primind_owner":
			if !seenSource {
				t.Fatal("owner before source")
			}
		case "primind_from", "primind_to":
			if !seenRelation {
				t.Fatal("edge before relation")
			}
		}
		if strings.Contains(b.Statement, "第二行") {
			t.Fatal("raw query syntax leaked")
		}
	}
	if len(plan[0].IDs) != 2 {
		t.Fatal("not batched")
	}
	huge := sampleGraph()
	huge.Node[0].Attributes = []string{strings.Repeat("中", 100000)}
	w.stmts = nil
	requireErr(t, r.AddGraph(context.Background(), testNS(), []*types.GraphData{huge}), ErrBudget)
	if len(w.stmts) != 0 {
		t.Fatal("prevalidation wrote partial input")
	}
}
func TestP2RegistryLifecycleAndInitFailure(t *testing.T) {
	registry := &registry{entries: map[[32]byte]*poolEntry{}}
	cfg := testConfig()
	w := &testWire{}
	built := 0
	factory := func(context.Context, Config) (wireDriver, error) { built++; return w, nil }
	a, err := registry.open(context.Background(), cfg, factory)
	if err != nil {
		t.Fatal(err)
	}
	b, err := registry.open(context.Background(), cfg, factory)
	if err != nil || a.c != b.c || built != 1 {
		t.Fatal("pool not shared")
	}
	if err := a.Close(context.Background()); err != nil || w.closed.Load() != 0 {
		t.Fatal("premature shared close")
	}
	requireErr(t, a.AddGraph(context.Background(), testNS(), nil), ErrClosed)
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = b.Close(context.Background())
	if w.closed.Load() != 1 {
		t.Fatal("close not idempotent")
	}
	requireErr(t, b.DelGraph(context.Background(), nil), ErrClosed)
	_, err = b.SearchNode(context.Background(), testNS(), nil)
	requireErr(t, err, ErrClosed)
	bad := &testWire{}
	_, err = registry.open(context.Background(), cfg, func(context.Context, Config) (wireDriver, error) { return bad, errors.New("authentication failure") })
	if err == nil || bad.closed.Load() != 1 {
		t.Fatal("partial initialization leaked")
	}
}
func TestP3SharedAdmissionAndReservedRead(t *testing.T) {
	cfg := testConfig()
	cfg.Connections = 3
	cfg.Writes = 2
	cfg.Waiters = 2
	cfg.AcquireTimeout = time.Second
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	w := &testWire{fn: func(context.Context, string) wireOutcome {
		entered <- struct{}{}
		<-release
		return wireOutcome{Sent: true}
	}}
	r := testRepo(cfg, w)
	done := make(chan error, 3)
	for i := 0; i < 2; i++ {
		go func() {
			done <- r.AddGraph(context.Background(), testNS(), []*types.GraphData{{Node: []*types.GraphNode{{Name: "one"}}}})
		}()
		waitSignal(t, entered)
	}
	// A third write must wait, while a read can consume the reserved slot.
	blockedCtx, cancel := context.WithCancel(context.Background())
	blocked := make(chan wireOutcome, 1)
	go func() { blocked <- r.c.execute(blockedCtx, batch{Kind: "write", Statement: "write"}, true) }()
	go func() { _, err := r.read(context.Background(), "probe", "probe"); done <- err }()
	waitSignal(t, entered)
	m := r.Metrics()
	if m.Peak != 3 || m.PeakWrites != 2 {
		t.Fatalf("unexpected peaks %+v", m)
	}
	cancel()
	select {
	case o := <-blocked:
		requireErr(t, o.Err, context.Canceled)
		if o.Sent {
			t.Fatal("canceled waiter sent")
		}
	case <-time.After(time.Second):
		t.Fatal("wait cancel blocked")
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if r.Metrics().Active != 0 {
		t.Fatal("permit leaked")
	}
	// Queue capacity and bytes are enforced before launching an executor.
	free, _, err := r.c.admit(context.Background(), true, 10)
	if err != nil {
		t.Fatal(err)
	}
	free2, _, err := r.c.admit(context.Background(), true, 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx, c := context.WithCancel(context.Background())
	defer c()
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			f, _, _ := r.c.admit(ctx, true, 10)
			if f != nil {
				f()
			}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for r.Metrics().Waiting != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, _, err = r.c.admit(context.Background(), true, 10)
	requireErr(t, err, ErrCapacity)
	c()
	wg.Wait()
	free()
	free2()
	_, _, err = r.c.admit(context.Background(), true, cfg.PendingBytes+1)
	requireErr(t, err, ErrCapacity)
}
func TestP4CancelInFlightAndBoundedClose(t *testing.T) {
	cfg := testConfig()
	cfg.CloseTimeout = 20 * time.Millisecond
	entered, release := make(chan struct{}, 1), make(chan struct{})
	w := &testWire{fn: func(context.Context, string) wireOutcome {
		entered <- struct{}{}
		<-release
		return wireOutcome{Sent: true}
	}}
	r := testRepo(cfg, w)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.AddGraph(ctx, testNS(), []*types.GraphData{sampleGraph()}) }()
	waitSignal(t, entered)
	cancel()
	select {
	case <-done:
		t.Fatal("pretend RPC cancellation")
	case <-time.After(20 * time.Millisecond):
	}
	if r.Metrics().Active != 1 {
		t.Fatal("permit released while RPC active")
	}
	requireErr(t, r.Close(context.Background()), context.DeadlineExceeded)
	if w.closed.Load() != 0 {
		t.Fatal("closed active driver")
	}
	close(release)
	err := <-done
	requireErr(t, err, context.Canceled)
	var partial *OperationError
	if !errors.As(err, &partial) || partial.Batches[0].State != "unknown" {
		t.Fatal("unknown write state lost")
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.closed.Load() != 1 || r.Metrics().Active != 0 {
		t.Fatal("drain failed")
	}
}
func TestP5PartialFailureAndDeterministicReplay(t *testing.T) {
	cfg := testConfig()
	cfg.BatchRecords = 1
	calls := 0
	w := &testWire{fn: func(context.Context, string) wireOutcome {
		calls++
		if calls == 2 {
			return wireOutcome{Sent: true, Err: errors.New("response lost")}
		}
		return wireOutcome{Sent: true}
	}}
	r := testRepo(cfg, w)
	graphs := []*types.GraphData{sampleGraph()}
	err := r.AddGraph(context.Background(), testNS(), graphs)
	var partial *OperationError
	if !errors.As(err, &partial) || partial.Batches[0].State != "confirmed" || partial.Batches[1].State != "unknown" || partial.Batches[2].State != "not_sent" {
		t.Fatalf("wrong partial receipt: %v", err)
	}
	if calls != 2 {
		t.Fatal("new work dispatched after error")
	}
	first := append([]string(nil), w.stmts...)
	w.fn = nil
	if err := r.AddGraph(context.Background(), testNS(), graphs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, w.stmts[2:4]) {
		t.Fatal("replay identities changed")
	}
	for _, b := range partial.Batches {
		for _, id := range b.IdentityDigests {
			if len(id) != 64 {
				t.Fatal("raw identity in receipt")
			}
		}
	}
}
func TestSDKScalarDecodeAndReadBudgets(t *testing.T) {
	response := &gr.ExecutionResponse{ErrorCode: nv.ErrorCode_SUCCEEDED, Data: &nv.DataSet{ColumnNames: [][]byte{[]byte("name")}, Rows: []*nv.Row{{Values: []*nv.Value{{SVal: []byte(encoded("泵"))}}}}}}
	rs, err := ng.GenResultSet(response)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeSDKTable(rs, testConfig())
	if err != nil || out.Rows[0][0] != encoded("泵") {
		t.Fatal("SDK decoding")
	}
	cfg := testConfig()
	cfg.ReadBytes = 1
	_, err = decodeSDKTable(rs, cfg)
	requireErr(t, err, ErrBudget)
	response.Data.Rows[0].Values[0] = &nv.Value{LVal: &nv.NList{}}
	rs, _ = ng.GenResultSet(response)
	_, err = decodeSDKTable(rs, testConfig())
	requireErr(t, err, ErrInvalid)
}

type goldenKey struct {
	KB   string `json:"knowledge_base"`
	Doc  string `json:"knowledge"`
	Name string `json:"name"`
}
type goldenNode struct {
	Key                goldenKey `json:"entity_key"`
	Chunks, Attributes []string
}
type goldenRelation struct {
	KB                 string `json:"knowledge_base"`
	Doc                string `json:"knowledge"`
	Node1, Node2, Type string
}
type goldenState struct {
	Nodes     []goldenNode
	Relations []goldenRelation `json:"relation_keys"`
}
type goldenCheck struct {
	ID         string
	Namespace  types.NameSpace
	QueryNames []string `json:"query_names"`
	Expected   goldenState
	First      *goldenKey `json:"first_entity_key"`
}
type goldenFile struct {
	Baseline goldenState
	Checks   []goldenCheck
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	b, e := os.ReadFile("testdata/ng01_expected.json")
	if e != nil {
		t.Fatal(e)
	}
	var g goldenFile
	if e = json.Unmarshal(b, &g); e != nil {
		t.Fatal(e)
	}
	return g
}

// This wire transcript is populated from A's fixed independent baseline, not
// from AddGraph or a storage emulator. Only transport decoding/search is tested.
func goldenWire(t *testing.T, cfg Config, state goldenState, ns types.NameSpace) *testWire {
	tables := map[string]wireTable{}
	edges := map[string]map[string][]string{"primind_owner": {}, "primind_from": {}, "primind_to": {}}
	addEdge := func(kind, src, dst string) { edges[kind][literal(src)+"->"+literal(dst)] = []string{src, dst} }
	for _, tag := range []string{entityTag, relationTag, sourceTag} {
		tables[tag] = wireTable{Columns: append([]string{"vid"}, tagFields[tag]...)}
	}
	appendRow := func(tag, id string, fields ...string) {
		tb := tables[tag]
		row := []string{id}
		for _, v := range fields {
			row = append(row, encoded(v))
		}
		tb.Rows = append(tb.Rows, row)
		tables[tag] = tb
	}
	inScope := func(kb, doc string) bool {
		return kb == ns.KnowledgeBase && (ns.Knowledge == "" || doc == ns.Knowledge)
	}
	for _, n := range state.Nodes {
		if !inScope(n.Key.KB, n.Key.Doc) {
			continue
		}
		scope := types.NameSpace{KnowledgeBase: n.Key.KB, Knowledge: n.Key.Doc}
		id, _ := identity(cfg, "entity", scope, n.Key.Name)
		attrs, _ := json.Marshal(n.Attributes)
		appendRow(entityTag, id, n.Key.KB, n.Key.Doc, n.Key.Name, string(attrs))
		for _, chunk := range n.Chunks {
			sid, _ := identity(cfg, "source", scope, n.Key.Name, chunk)
			appendRow(sourceTag, sid, n.Key.KB, n.Key.Doc, n.Key.Name, chunk)
			addEdge("primind_owner", sid, id)
		}
	}
	for _, e := range state.Relations {
		if !inScope(e.KB, e.Doc) {
			continue
		}
		scope := types.NameSpace{KnowledgeBase: e.KB, Knowledge: e.Doc}
		id, _ := identity(cfg, "relation", scope, e.Node1, e.Node2, e.Type)
		appendRow(relationTag, id, e.KB, e.Doc, e.Node1, e.Node2, e.Type)
		src, _ := identity(cfg, "entity", scope, e.Node1)
		dst, _ := identity(cfg, "entity", scope, e.Node2)
		addEdge("primind_from", id, src)
		addEdge("primind_to", id, dst)
	}
	return &testWire{fn: func(_ context.Context, s string) wireOutcome {
		for kind, pairs := range edges {
			if strings.HasPrefix(s, "LOOKUP ON "+kind+" ") {
				if s != edgeLookup(cfg, kind, ns) {
					t.Error("edge query scope mismatch")
				}
				tb := wireTable{Columns: edgeColumns}
				for _, pair := range pairs {
					fields, e := parseIdentity(cfg, pair[0])
					if e != nil {
						t.Fatal(e)
					}
					tb.Rows = append(tb.Rows, []string{pair[0], pair[1], "0", encoded(fields[1]), encoded(fields[2])})
				}
				return wireOutcome{Table: tb, Sent: true}
			}
		}
		for tag, tb := range tables {
			if strings.HasPrefix(s, "LOOKUP ON "+tag+" ") {
				if s != lookup(cfg, tag, ns) {
					t.Errorf("query scope mismatch")
				}
				return wireOutcome{Table: tb, Sent: true}
			}
		}
		if strings.HasPrefix(s, "FETCH PROP ON ") {
			fields := strings.SplitN(s, " ", 5)
			body := strings.Split(fields[4], " YIELD ")[0]
			tb := wireTable{Columns: []string{"src", "dst"}}
			for _, pair := range strings.Split(body, ",") {
				ends := strings.Split(pair, "->")
				if len(ends) != 2 {
					t.Fatal("invalid FETCH shape")
				}
				if row, ok := edges[fields[3]][pair]; ok {
					tb.Rows = append(tb.Rows, row)
				}
			}
			return wireOutcome{Table: tb, Sent: true}
		}
		t.Errorf("unexpected protocol statement")
		return wireOutcome{Err: ErrInvalid}
	}}
}
func canonicalGraph(g *types.GraphData, kb string) []string {
	var out []string
	docs := map[string]string{}
	for _, n := range g.Node {
		docs[n.ID] = n.KnowledgeID
		chunks := append([]string(nil), n.Chunks...)
		sort.Strings(chunks)
		attrs := n.Attributes
		if attrs == nil {
			attrs = []string{}
		}
		b, _ := json.Marshal([]any{kb, n.KnowledgeID, n.Name, chunks, attrs})
		out = append(out, "N"+string(b))
	}
	for _, e := range g.Relation {
		b, _ := json.Marshal([]string{kb, docs[e.SourceID], e.Node1, e.Node2, e.Type})
		out = append(out, "R"+string(b))
	}
	sort.Strings(out)
	return out
}
func canonicalGolden(s goldenState) []string {
	var out []string
	for _, n := range s.Nodes {
		chunks := append([]string(nil), n.Chunks...)
		sort.Strings(chunks)
		attrs := n.Attributes
		if attrs == nil {
			attrs = []string{}
		}
		b, _ := json.Marshal([]any{n.Key.KB, n.Key.Doc, n.Key.Name, chunks, attrs})
		out = append(out, "N"+string(b))
	}
	for _, e := range s.Relations {
		b, _ := json.Marshal([]string{e.KB, e.Doc, e.Node1, e.Node2, e.Type})
		out = append(out, "R"+string(b))
	}
	sort.Strings(out)
	return out
}
func TestFixedQueriesAgainstIndependentProtocolBaseline(t *testing.T) {
	g := loadGolden(t)
	cfg := testConfig()
	for _, q := range g.Checks {
		if !strings.HasPrefix(q.ID, "Q") {
			continue
		}
		t.Run(q.ID, func(t *testing.T) {
			r := testRepo(cfg, goldenWire(t, cfg, g.Baseline, q.Namespace))
			got, err := r.SearchNode(context.Background(), q.Namespace, q.QueryNames)
			if err != nil || got == nil {
				t.Fatalf("query: %v", err)
			}
			if !reflect.DeepEqual(canonicalGraph(got, q.Namespace.KnowledgeBase), canonicalGolden(q.Expected)) {
				t.Fatalf("fixed expected mismatch\ngot %v\nwant %v", canonicalGraph(got, q.Namespace.KnowledgeBase), canonicalGolden(q.Expected))
			}
			if q.First != nil && (len(got.Node) == 0 || got.Node[0].Name != q.First.Name || got.Node[0].KnowledgeID != q.First.Doc) {
				t.Fatal("Q1 raw first seed")
			}
		})
	}
}
func TestDeleteScopeMalformedResponsesAndNoSilentTruncation(t *testing.T) {
	cfg := testConfig()
	g := loadGolden(t)
	ns := testNS()
	w := goldenWire(t, cfg, g.Baseline, ns)
	read := w.fn
	w.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.HasPrefix(s, "DELETE EDGE ") {
			return wireOutcome{Sent: true}
		}
		if strings.HasPrefix(s, "DELETE VERTEX ") {
			if !strings.HasSuffix(s, " WITH EDGE;") {
				t.Fatal("missing edge cleanup")
			}
			return wireOutcome{Sent: true}
		}
		return read(ctx, s)
	}
	r := testRepo(cfg, w)
	if err := r.DelGraph(context.Background(), []types.NameSpace{ns}); err != nil {
		t.Fatal(err)
	}
	requireErr(t, r.DelGraph(context.Background(), []types.NameSpace{{KnowledgeBase: "kb-a"}}), ErrInvalid)
	cfg.SnapshotRecords = 2
	r = testRepo(cfg, goldenWire(t, cfg, g.Baseline, ns))
	_, err := r.Snapshot(context.Background(), ns)
	requireErr(t, err, ErrBudget)
	bad := &testWire{fn: func(context.Context, string) wireOutcome {
		return wireOutcome{Sent: true, Table: wireTable{Columns: []string{"wrong"}}}
	}}
	_, err = testRepo(testConfig(), bad).SearchNode(context.Background(), ns, []string{"泵"})
	requireErr(t, err, ErrInvalid)
}
func TestSchemaCreationRequiresExplicitScopeBeforeNetwork(t *testing.T) {
	err := InitializeSchema(context.Background(), testConfig(), SchemaAuthorization{})
	requireErr(t, err, ErrInvalid)
	for _, stmt := range SchemaDDL() {
		if strings.Contains(stmt, "SPACE") || strings.Contains(stmt, "DROP") || strings.Contains(stmt, "ALTER") {
			t.Fatal("unexpected destructive DDL")
		}
	}
}
func ExampleOperationError() {
	fmt.Println("confirmed / unknown / not_sent") // Output: confirmed / unknown / not_sent
}
