package nebula

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
	ng "github.com/vesoft-inc/nebula-go/v3"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
)

type testWire struct {
	fn     func(context.Context, string) wireOutcome
	mu     sync.Mutex
	stmts  []string
	closed atomic.Int32
}

// Execute renders parameters into the statement as escaped nGQL literals so
// test transcripts stay self-contained and existing fakes see one string.
func (w *testWire) Execute(ctx context.Context, s string, params map[string]string) wireOutcome {
	s = renderParams(s, params)
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
	return &types.GraphData{Node: []*types.GraphNode{{
		Name: "泵\"Q\\X\n第二行", Chunks: []string{"c1", "c2"},
		Attributes: []string{"a\n\""},
	}, {Name: "阀"}}, Relation: []*types.GraphRelation{{
		Node1: "泵\"Q\\X\n第二行",
		Node2: "阀", Type: "连接",
	}, {Node1: "泵\"Q\\X\n第二行", Node2: "阀", Type: "反馈"}}}
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
	// 泵 is U+6CF5 (UTF-8 E6 B3 B5), not U+6CF3. Independent vector: the first
	// 20 bytes of SHA-256 over the compact JSON ["entity","kb-a","doc-a1","主泵A"],
	// base64url without padding, computed outside this package.
	const known = "ng2_70Ec4ChTjam6Rfp7QgkSCNMj6vw"
	if id != known || !safeVID(id) {
		t.Fatalf("fixed identity vector: %s", id)
	}
	// Fixed length whatever the name is, and only the exact shape is accepted.
	long, err := identity(cfg, "entity", ns, strings.Repeat("超长名称", 2000))
	if err != nil || len(long) != vidLength || !safeVID(long) {
		t.Fatalf("long name identity: %v %q", err, long)
	}
	for _, bad := range []string{
		"", "ng1_" + id[4:], id[:len(id)-1], id + "A", id[:10] + "\"" + id[11:], id[:10] + " " + id[11:],
	} {
		if safeVID(bad) {
			t.Fatalf("safeVID accepted %q", bad)
		}
	}
	keys := map[string]bool{}
	for _, n := range []types.NameSpace{ns, {KnowledgeBase: "kb_a", Knowledge: ns.Knowledge}, {
		KnowledgeBase: "kb-a",
		Knowledge:     "doc-a2",
	}} {
		for _, role := range []string{"entity", "source", "relation"} {
			x, e := identity(cfg, role, n, "A|B", "C")
			if e != nil || keys[x] {
				t.Fatalf("collision/error %v", e)
			}
			keys[x] = true
		}
	}
	for in, want := range map[string]string{
		"Dexamethasone": "dexamethasone",
		"ｄｅｘａｍｅｔｈａｓｏｎｅ":               "dexamethasone",
		"ＤＥＸＡＭＥＴＨＡＳＯＮＥ":               "dexamethasone",
		"type ２  diabetes\tmellitus ": "type 2 diabetes mellitus",
		"地塞米松":                        "地塞米松",
		"　全角空格　":                      "全角空格",
	} {
		if got := nameKey(in); got != want {
			t.Fatalf("nameKey(%q) = %q, want %q", in, got, want)
		}
	}
	// Parameters, not text: user values never appear in the statement.
	evil := "a'); DROP SPACE x; --"
	bs, err := splitBatches(cfg, entityTag, vertexPrefix(entityTag), ";",
		[]record{vertexRecord(id, "kb-a", "doc-a1", evil, "k", "[]")})
	if err != nil || len(bs) != 1 || strings.Contains(bs[0].Statement, "DROP") || bs[0].Params["p0_2"] != evil {
		t.Fatalf("parameterised insert: %v %+v", err, bs)
	}
	for _, v := range []string{"泵\"Q\\X\n第二行", "\x00\x01\t\r\n", "\x0012", "中文😀"} {
		q := quoteNGQL(v)
		if strings.ContainsAny(q[1:len(q)-1], "\x00\n\r\x01") {
			t.Fatalf("raw control character in literal %q", q)
		}
	}
	// A name that overflowed the old reversible VID no longer fails: the VID
	// is a fixed-length digest. Invalid UTF-8 is still refused.
	if _, err = identity(cfg, "entity", ns, strings.Repeat("长", 1000)); err != nil {
		t.Fatalf("long name: %v", err)
	}
	_, err = identity(cfg, "entity", ns, "\xff")
	requireErr(t, err, ErrInvalid)
	requireErr(t, validateNamespace(types.NameSpace{}, false), ErrInvalid)
}

func TestP1BatchLimitsAndDependency(t *testing.T) {
	cfg := testConfig()
	cfg.BatchRecords = 2
	cfg.StatementBytes = 256
	rs := []record{
		{ID: "a", Value: strings.Repeat("中", 30)}, {ID: "b", Value: strings.Repeat("x", 100)}, {ID: "c", Value: "ok"},
	}
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
	_, err = splitBatches(cfg, "test", "PREFIX ", ";", []record{{ID: "big", Value: strings.Repeat("中", 100)}})
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
	_, err = registry.open(context.Background(), cfg, func(context.Context, Config) (wireDriver, error) {
		return bad,
			errors.New("authentication failure")
	})
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
			done <- r.AddGraph(context.Background(), testNS(),
				[]*types.GraphData{{Node: []*types.GraphNode{{Name: "one"}}}})
		}()
		waitSignal(t, entered)
	}
	// A third write must wait, while a read can consume the reserved slot.
	blockedCtx, cancel := context.WithCancel(context.Background())
	blocked := make(chan wireOutcome, 1)
	go func() { blocked <- r.c.execute(blockedCtx, batch{Kind: "write", Statement: "write"}, true) }()
	go func() { _, err := r.read(context.Background(), "probe", "probe", nil); done <- err }()
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
	if !errors.As(err,
		&partial) || partial.Batches[0].State != "confirmed" || partial.Batches[1].State != "unknown" ||
		partial.Batches[2].State != "not_sent" {
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
	response := &gr.ExecutionResponse{
		ErrorCode: nv.ErrorCode_SUCCEEDED,
		Data: &nv.DataSet{
			ColumnNames: [][]byte{[]byte("name")},
			Rows:        []*nv.Row{{Values: []*nv.Value{{SVal: []byte("泵")}}}},
		},
	}
	rs, err := ng.GenResultSet(response)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeSDKTable(rs, testConfig())
	if err != nil || out.Rows[0][0] != "泵" {
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

// quoteNGQL is the nGQL string-literal escaper verified against NebulaGraph
// 3.8.0: control characters must use the fixed-width \u
// form because the lexer corrupts raw NUL and reads \0 as an octal escape.
func quoteNGQL(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, "\\u%04x", r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ngqlStrings returns the string literals in s, undoing quoteNGQL.
func ngqlStrings(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		var b strings.Builder
		for i++; i < len(s) && s[i] != '"'; i++ {
			if s[i] != '\\' || i+1 >= len(s) {
				b.WriteByte(s[i])
				continue
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if i+4 < len(s) {
					if n, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
						b.WriteRune(rune(n))
						i += 4
					}
				}
			default:
				b.WriteByte(s[i])
			}
		}
		out = append(out, b.String())
	}
	return out
}

var paramPattern = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)

func renderParams(s string, params map[string]string) string {
	if len(params) == 0 {
		return s
	}
	return paramPattern.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := params[m[1:]]; ok {
			return quoteNGQL(v)
		}
		return m
	})
}

func renderedLookup(cfg Config, tag string, ns types.NameSpace) string {
	s, p := lookup(cfg, tag, ns)
	return renderParams(s, p)
}

func renderedEdgeLookup(cfg Config, kind string, ns types.NameSpace) string {
	s, p := edgeLookup(cfg, kind, ns)
	return renderParams(s, p)
}

// vidList parses `"a","b"` (VID literals only).
func vidList(t *testing.T, body string) []string {
	var out []string
	for _, part := range strings.Split(strings.TrimSpace(body), ",") {
		part = strings.TrimSpace(part)
		if len(part) < 2 || part[0] != '"' || part[len(part)-1] != '"' || !safeVID(part[1:len(part)-1]) {
			t.Fatalf("invalid VID list element %q", part)
		}
		out = append(out, part[1:len(part)-1])
	}
	return out
}

// goldenWire answers the adapter's fixed statement shapes from A's independent
// baseline (not from AddGraph): scoped LOOKUPs, the seed scan, GO expansions
// and FETCH PROP by VID. It is a protocol transcript, not a query engine.
func goldenWire(t *testing.T, cfg Config, state goldenState, ns types.NameSpace) *testWire {
	vertices := map[string]map[string][]string{entityTag: {}, relationTag: {}, sourceTag: {}}
	order := map[string][]string{}
	edges := map[string][][4]string{} // src, dst, kb, doc
	addVertex := func(tag, id string, fields ...string) {
		if _, ok := vertices[tag][id]; !ok {
			order[tag] = append(order[tag], id)
		}
		vertices[tag][id] = fields
	}
	addEdge := func(kind, src, dst string, n types.NameSpace) {
		edges[kind] = append(edges[kind], [4]string{src, dst, n.KnowledgeBase, n.Knowledge})
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
		addVertex(entityTag, id, n.Key.KB, n.Key.Doc, n.Key.Name, nameKey(n.Key.Name), string(attrs))
		for _, chunk := range n.Chunks {
			sid, _ := identity(cfg, "source", scope, n.Key.Name, chunk)
			addVertex(sourceTag, sid, n.Key.KB, n.Key.Doc, n.Key.Name, chunk)
			addEdge("primind_owner", sid, id, scope)
		}
	}
	for _, e := range state.Relations {
		if !inScope(e.KB, e.Doc) {
			continue
		}
		scope := types.NameSpace{KnowledgeBase: e.KB, Knowledge: e.Doc}
		id, _ := identity(cfg, "relation", scope, e.Node1, e.Node2, e.Type)
		addVertex(relationTag, id, e.KB, e.Doc, e.Node1, e.Node2, e.Type)
		src, _ := identity(cfg, "entity", scope, e.Node1)
		dst, _ := identity(cfg, "entity", scope, e.Node2)
		addEdge("primind_from", id, src, scope)
		addEdge("primind_to", id, dst, scope)
	}
	where, scopeParams := scopeWhere(entityTag, ns)
	seedHead := renderParams("LOOKUP ON "+entityTag+" WHERE "+where, scopeParams)
	seedShape := regexp.MustCompile(`^(.*?) YIELD id\(vertex\) AS vid,` + entityTag + `\.doc AS doc,` + entityTag +
		`\.name AS name,` + entityTag + `\.name_key AS name_key \| (.*)LIMIT (\d+);$`)
	goShape := regexp.MustCompile(`^GO FROM (.+) OVER ([a-z_,]+)( REVERSELY)? YIELD (.+);$`)
	fetchShape := regexp.MustCompile(`^FETCH PROP ON ([a-z_]+) (.+) YIELD (.+);$`)
	return &testWire{fn: func(_ context.Context, s string) wireOutcome {
		for kind, pairs := range edges {
			if strings.HasPrefix(s, "LOOKUP ON "+kind+" ") {
				if s != renderedEdgeLookup(cfg, kind, ns) {
					t.Error("edge query scope mismatch")
				}
				tb := wireTable{Columns: edgeColumns}
				for _, pair := range pairs {
					tb.Rows = append(tb.Rows, []string{pair[0], pair[1], "0", pair[2], pair[3]})
				}
				return wireOutcome{Table: tb, Sent: true}
			}
		}
		if m := seedShape.FindStringSubmatch(s); m != nil {
			// The three seed statements: exact (IN through the index), prefix
			// (STARTS WITH through the index) and the in-store substring
			// filter. Each sorts, then cuts at LIMIT.
			cond := strings.TrimPrefix(m[1], seedHead)
			if !strings.HasPrefix(m[1], seedHead) {
				t.Errorf("seed statement scope mismatch: %s", s)
			}
			pipe := m[2]
			type seed struct{ vid, doc, name, key string }
			var keep func(seed) bool
			var less func(a, b seed) bool
			tb := wireTable{Columns: []string{"vid", "doc", "name", "name_key"}}
			switch {
			case strings.HasPrefix(cond, " AND "+entityTag+".name_key IN [") && pipe == "ORDER BY $-.doc,$-.vid | ":
				want := ngqlStrings(cond)
				keep = func(x seed) bool {
					for _, w := range want {
						if x.key == w {
							return true
						}
					}
					return false
				}
				less = func(a, b seed) bool {
					if a.doc != b.doc {
						return a.doc < b.doc
					}
					return a.vid < b.vid
				}
			case strings.HasPrefix(cond, " AND "+entityTag+".name_key >= ") &&
				strings.HasPrefix(pipe, "YIELD $-.vid AS vid,$-.doc AS doc,$-.name AS name,$-.name_key AS name_key"+
					" WHERE $-.name_key STARTS WITH ") &&
				strings.HasSuffix(pipe, " | ORDER BY $-.name_key,$-.doc,$-.vid | "):
				// Prefix fallback: an index range [lo, hi) (hi optional), then
				// STARTS WITH on the full key. Both are applied as written, so
				// bounds that excluded a match would lose it here too.
				bounds, prefix := ngqlStrings(cond), ngqlStrings(pipe)
				hasHi := strings.Contains(cond, " AND "+entityTag+".name_key < ")
				if len(prefix) != 1 || len(bounds) != map[bool]int{false: 1, true: 2}[hasHi] {
					t.Errorf("prefix statement shape: %s", s)
				}
				keep = func(x seed) bool {
					inRange := x.key >= bounds[0] && (!hasHi || x.key < bounds[1])
					return inRange && len(prefix) == 1 && strings.HasPrefix(x.key, prefix[0])
				}
				less = func(a, b seed) bool {
					if a.key != b.key {
						return a.key < b.key
					}
					if a.doc != b.doc {
						return a.doc < b.doc
					}
					return a.vid < b.vid
				}
			case cond == "" && strings.HasPrefix(pipe, "YIELD $-.vid AS vid,$-.doc AS doc,$-.name AS name,"+
				"$-.name_key AS name_key,length($-.name) AS len WHERE $-.name_key CONTAINS ") &&
				strings.HasSuffix(pipe, " | ORDER BY $-.len,$-.name,$-.doc,$-.vid | "):
				want := ngqlStrings(pipe)
				if strings.Count(pipe, "$-.name_key CONTAINS ") != len(want) {
					t.Errorf("substring filter shape: %s", s)
				}
				tb.Columns = append(tb.Columns, "len")
				keep = func(x seed) bool {
					for _, w := range want {
						if strings.Contains(x.key, w) {
							return true
						}
					}
					return false
				}
				less = func(a, b seed) bool { // length() counts bytes in NebulaGraph
					if len(a.name) != len(b.name) {
						return len(a.name) < len(b.name)
					}
					if a.name != b.name {
						return a.name < b.name
					}
					if a.doc != b.doc {
						return a.doc < b.doc
					}
					return a.vid < b.vid
				}
			default:
				t.Errorf("unexpected seed statement %q", s)
				return wireOutcome{Err: ErrInvalid}
			}
			var found []seed
			for _, id := range order[entityTag] {
				f := vertices[entityTag][id]
				if x := (seed{id, f[1], f[2], f[3]}); keep(x) {
					found = append(found, x)
				}
			}
			sort.Slice(found, func(i, j int) bool { return less(found[i], found[j]) })
			limit, _ := strconv.Atoi(m[3])
			if len(found) > limit {
				found = found[:limit]
			}
			for _, x := range found {
				row := []string{x.vid, x.doc, x.name, x.key}
				if len(tb.Columns) == 5 {
					row = append(row, strconv.Itoa(len(x.name)))
				}
				tb.Rows = append(tb.Rows, row)
			}
			return wireOutcome{Table: tb, Sent: true}
		}
		for tag := range vertices {
			if strings.HasPrefix(s, "LOOKUP ON "+tag+" ") {
				if s != renderedLookup(cfg, tag, ns) {
					t.Errorf("query scope mismatch")
				}
				tb := wireTable{Columns: append([]string{"vid"}, tagFields[tag]...)}
				for _, id := range order[tag] {
					tb.Rows = append(tb.Rows, append([]string{id}, vertices[tag][id]...))
				}
				return wireOutcome{Table: tb, Sent: true}
			}
		}
		if m := goShape.FindStringSubmatch(s); m != nil {
			from := map[string]bool{}
			for _, v := range vidList(t, m[1]) {
				from[v] = true
			}
			reverse := m[3] != ""
			var exprs []string
			tb := wireTable{}
			for _, item := range strings.Split(m[4], ",") {
				parts := strings.SplitN(item, " AS ", 2)
				if len(parts) != 2 {
					t.Fatalf("unexpected GO yield %q", item)
				}
				exprs = append(exprs, parts[0])
				tb.Columns = append(tb.Columns, parts[1])
			}
			prop := func(vid, tag, field string) string {
				fields, ok := vertices[tag][vid]
				if !ok {
					return "" // absent vertex: properties come back empty
				}
				for i, f := range tagFields[tag] {
					if f == field {
						return fields[i]
					}
				}
				t.Fatalf("unknown property %s.%s", tag, field)
				return ""
			}
			for _, kind := range strings.Split(m[2], ",") {
				for _, pair := range edges[kind] {
					start, reached := pair[0], pair[1]
					if reverse {
						start, reached = pair[1], pair[0]
					}
					if !from[start] {
						continue
					}
					row := make([]string, len(exprs))
					for i, e := range exprs {
						switch {
						case e == "src(edge)":
							row[i] = pair[0]
						case e == "dst(edge)":
							row[i] = pair[1]
						case e == "type(edge)":
							row[i] = kind
						case e == "rank(edge)":
							row[i] = "0"
						case strings.HasPrefix(e, "$^."):
							ref := strings.Split(e[3:], ".")
							row[i] = prop(start, ref[0], ref[1])
						case strings.HasPrefix(e, "$$."):
							ref := strings.Split(e[3:], ".")
							row[i] = prop(reached, ref[0], ref[1])
						default:
							t.Fatalf("unexpected GO expression %q", e)
						}
					}
					tb.Rows = append(tb.Rows, row)
				}
			}
			return wireOutcome{Table: tb, Sent: true}
		}
		if m := fetchShape.FindStringSubmatch(s); m != nil {
			if _, vertex := vertices[m[1]]; vertex {
				tb := wireTable{Columns: []string{"vid"}}
				owner := m[3] == "id(vertex) AS vid,"+m[1]+".kb AS kb,"+m[1]+".doc AS doc"
				full := !owner && strings.HasPrefix(m[3], "id(vertex) AS vid,")
				if owner {
					tb.Columns = []string{"vid", "kb", "doc"}
				}
				if full {
					tb.Columns = append(tb.Columns, tagFields[m[1]]...)
				}
				for _, id := range vidList(t, m[2]) {
					fields, ok := vertices[m[1]][id]
					if !ok {
						continue
					}
					if owner {
						tb.Rows = append(tb.Rows, []string{id, fields[0], fields[1]})
						continue
					}
					if full {
						tb.Rows = append(tb.Rows, append([]string{id}, fields...))
					} else {
						tb.Rows = append(tb.Rows, []string{id})
					}
				}
				return wireOutcome{Table: tb, Sent: true}
			}
			tb := wireTable{Columns: []string{"src", "dst"}}
			for _, pair := range strings.Split(m[2], ",") {
				ends := strings.Split(pair, "->")
				if len(ends) != 2 {
					t.Fatal("invalid FETCH shape")
				}
				for _, e := range edges[m[1]] {
					if literal(e[0]) == ends[0] && literal(e[1]) == ends[1] {
						tb.Rows = append(tb.Rows, []string{e[0], e[1]})
					}
				}
			}
			return wireOutcome{Table: tb, Sent: true}
		}
		t.Errorf("unexpected protocol statement %q", s)
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
		if q.ID == "Q10" {
			// Layout v2 deliberately matches on the normalised name_key (NFKC +
			// lower case). The shared golden pins the case-sensitive behaviour
			// that Neo4j keeps, so v2 asserts "主泵a" behaves like Q1 "主泵A".
			for _, q1 := range g.Checks {
				if q1.ID == "Q1" && q1.Namespace == q.Namespace {
					q.Expected, q.First = q1.Expected, q1.First
				}
			}
		}
		t.Run(q.ID, func(t *testing.T) {
			r := testRepo(cfg, goldenWire(t, cfg, g.Baseline, q.Namespace))
			got, err := r.SearchNode(context.Background(), q.Namespace, q.QueryNames)
			if err != nil || got == nil {
				t.Fatalf("query: %v", err)
			}
			if !reflect.DeepEqual(canonicalGraph(got, q.Namespace.KnowledgeBase), canonicalGolden(q.Expected)) {
				t.Fatalf("fixed expected mismatch\ngot %v\nwant %v", canonicalGraph(got, q.Namespace.KnowledgeBase),
					canonicalGolden(q.Expected))
			}
			if q.First != nil && (len(got.Node) == 0 || got.Node[0].Name != q.First.Name ||
				got.Node[0].KnowledgeID != q.First.Doc) {
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
