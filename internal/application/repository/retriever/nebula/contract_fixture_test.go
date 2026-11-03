package nebula_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/primind/primind/internal/types"
)

// These are fixture metadata, not replacement business DTOs or a Repository.
type fixtureCall struct {
	ID        string             `json:"id"`
	Namespace types.NameSpace    `json:"namespace"`
	Graphs    []*types.GraphData `json:"graphs"`
}
type fixtureInput struct {
	SchemaVersion int `json:"schema_version"`
	Constants     struct {
		S string `json:"S"`
		T string `json:"T"`
	} `json:"constants"`
	Cases        []fixtureCall `json:"cases"`
	Supplemental []fixtureCall `json:"supplemental_cases"`
}
type entityKey struct {
	KB   string `json:"knowledge_base"`
	Doc  string `json:"knowledge"`
	Name string `json:"name"`
}
type relationKey struct {
	KB    string `json:"knowledge_base"`
	Doc   string `json:"knowledge"`
	Node1 string `json:"node1"`
	Node2 string `json:"node2"`
	Type  string `json:"type"`
}
type fixtureNode struct {
	Key        entityKey `json:"entity_key"`
	Chunks     []string  `json:"chunks"`
	Attributes []string  `json:"attributes"`
}
type fixtureState struct {
	Entities  []entityKey   `json:"entity_keys"`
	Relations []relationKey `json:"relation_keys"`
	Nodes     []fixtureNode `json:"nodes"`
	Connected int           `json:"connected_entity_count"`
}
type fixtureCheck struct {
	ID                  string            `json:"id"`
	Operation           string            `json:"operation"`
	Precondition        string            `json:"precondition"`
	InputRefs           []string          `json:"input_refs"`
	Namespace           *types.NameSpace  `json:"namespace"`
	QueryNames          []string          `json:"query_names"`
	Expected            fixtureState      `json:"expected"`
	Empty               bool              `json:"expected_empty"`
	NonNil              bool              `json:"require_non_nil"`
	First               *entityKey        `json:"first_entity_key"`
	NeighborOrder       string            `json:"neighbor_order"`
	ForbiddenNamespaces []types.NameSpace `json:"forbidden_namespaces"`
	ForbiddenChunks     []string          `json:"forbidden_chunks"`
	DeleteNamespaces    []types.NameSpace `json:"delete_namespaces"`
	PreservedNamespaces []types.NameSpace `json:"preserved_namespaces"`
	EmptyQueries        []string          `json:"empty_query_ids"`
	NoSourceResidue     bool              `json:"requires_no_source_residue"`
}
type fixtureExpected struct {
	SchemaVersion  int            `json:"schema_version"`
	DatabaseStatus string         `json:"real_database_status"`
	Baseline       fixtureState   `json:"baseline"`
	Checks         []fixtureCheck `json:"checks"`
}

const specialName = "泵\"Q\\X\n第二行"
const specialType = "标记\"型\\X"
const specialAttribute = "说明:\"引号\"\\路径\n结束"

var a1 = types.NameSpace{KnowledgeBase: "kb-a", Knowledge: "doc-a1"}
var a2 = types.NameSpace{KnowledgeBase: "kb-a", Knowledge: "doc-a2"}
var b1 = types.NameSpace{KnowledgeBase: "kb-b", Knowledge: "doc-b1"}

func strictDecode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON value, trailing decode: %v", err)
	}
	return nil
}

func loadFixtures(t *testing.T) (fixtureInput, fixtureExpected) {
	t.Helper()
	var in fixtureInput
	var out fixtureExpected
	for _, f := range []struct {
		path   string
		target any
	}{
		{"testdata/ng01_graph_cases.json", &in}, {"testdata/ng01_expected.json", &out},
	} {
		data, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := strictDecode(data, f.target); err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
	}
	return in, out
}

// A manually reviewed transcription of NG-01A §3–4. No name matching, graph
// traversal, SDK, production mapper, or fixture input is used to calculate it.
func oracleRecords() ([]fixtureNode, []relationKey) {
	n := []fixtureNode{
		{entityKey{"kb-a", "doc-a1", "主泵A"}, []string{"c-a1-01", "c-a1-02"}, []string{"型号:PX-1"}},
		{entityKey{"kb-a", "doc-a1", "调节阀B"}, []string{"c-a1-01"}, []string{}},
		{entityKey{"kb-a", "doc-a1", "主泵A-备用"}, []string{"c-a1-03"}, []string{"备用"}},
		{entityKey{"kb-a", "doc-a1", "孤立标签"}, []string{"c-a1-04"}, []string{"独立"}},
		{entityKey{"kb-a", "doc-a1", specialName}, []string{"c-a1-special"}, []string{specialAttribute}},
		{entityKey{"kb-a", "doc-a2", "主泵A"}, []string{"c-a2-01"}, []string{"型号:PX-2"}},
		{entityKey{"kb-a", "doc-a2", "储罐C"}, []string{"c-a2-01"}, []string{}},
		{entityKey{"kb-b", "doc-b1", "主泵A"}, []string{"c-b1-01"}, []string{"型号:PX-3"}},
		{entityKey{"kb-b", "doc-b1", "调节阀B"}, []string{"c-b1-01"}, []string{}},
	}
	r := []relationKey{
		{"kb-a", "doc-a1", "主泵A", "调节阀B", "连接"},
		{"kb-a", "doc-a1", "主泵A", "调节阀B", "供液"},
		{"kb-a", "doc-a1", "调节阀B", "主泵A", "反馈"},
		{"kb-a", "doc-a1", "主泵A", "主泵A", "自检"},
		{"kb-a", "doc-a1", "主泵A-备用", "调节阀B", "旁路"},
		{"kb-a", "doc-a1", specialName, "调节阀B", specialType},
		{"kb-a", "doc-a2", "主泵A", "储罐C", "连接"},
		{"kb-b", "doc-b1", "主泵A", "调节阀B", "连接"},
	}
	return n, r
}

func oracleState(nodes, relations []int, connected int) fixtureState {
	n, r := oracleRecords()
	s := fixtureState{Entities: []entityKey{}, Relations: []relationKey{}, Nodes: []fixtureNode{}, Connected: connected}
	for _, i := range nodes {
		s.Entities = append(s.Entities, n[i].Key)
		s.Nodes = append(s.Nodes, n[i])
	}
	for _, i := range relations {
		s.Relations = append(s.Relations, r[i])
	}
	return s
}
func baselineOracle() fixtureState {
	return oracleState([]int{0, 1, 2, 3, 4, 5, 6, 7, 8}, []int{0, 1, 2, 3, 4, 5, 6, 7}, 8)
}

func inputOracle() fixtureInput {
	n, r := oracleRecords()
	makeCall := func(id string, ns types.NameSpace, ni, ri []int) fixtureCall {
		g := &types.GraphData{Node: []*types.GraphNode{}, Relation: []*types.GraphRelation{}}
		for _, i := range ni {
			g.Node = append(g.Node, &types.GraphNode{Name: n[i].Key.Name, Chunks: slices.Clone(n[i].Chunks), Attributes: slices.Clone(n[i].Attributes)})
		}
		for _, i := range ri {
			g.Relation = append(g.Relation, &types.GraphRelation{Node1: r[i].Node1, Node2: r[i].Node2, Type: r[i].Type})
		}
		return fixtureCall{id, ns, []*types.GraphData{g}}
	}
	in := fixtureInput{SchemaVersion: 1}
	in.Constants.S, in.Constants.T = specialName, specialType
	in.Cases = []fixtureCall{
		makeCall("A1-base", a1, []int{0, 1, 2, 3, 4}, []int{0, 1, 2, 3, 4, 5}),
		makeCall("A1-more-source", a1, []int{0}, nil),
		makeCall("A2", a2, []int{5, 6}, []int{6}), makeCall("B1", b1, []int{7, 8}, []int{7}),
	}
	in.Cases[0].Graphs[0].Node[0].Chunks = []string{"c-a1-01"}
	in.Cases[1].Graphs[0].Node[0].Chunks = []string{"c-a1-02"}
	in.Supplemental = []fixtureCall{makeCall("A1-concurrent-05", a1, []int{0}, nil), makeCall("A1-concurrent-06", a1, []int{0}, nil)}
	in.Supplemental[0].Graphs[0].Node[0].Chunks = []string{"c-a1-05"}
	in.Supplemental[1].Graphs[0].Node[0].Chunks = []string{"c-a1-06"}
	return in
}

func checkOracle() []fixtureCheck {
	refs := []string{"A1-base", "A1-more-source", "A2", "B1"}
	newCheck := func(id, op, pre string, st fixtureState) fixtureCheck {
		return fixtureCheck{ID: id, Operation: op, Precondition: pre, InputRefs: refs, QueryNames: []string{}, Expected: st, Empty: len(st.Entities) == 0,
			NeighborOrder: "unspecified", ForbiddenNamespaces: []types.NameSpace{}, ForbiddenChunks: []string{}, DeleteNamespaces: []types.NameSpace{}, PreservedNamespaces: []types.NameSpace{}, EmptyQueries: []string{}}
	}
	// Explicit memberships; numbers identify the manually declared records above.
	queries := []struct {
		id, name     string
		ns           types.NameSpace
		nodes, edges []int
	}{
		{"Q1", "主泵A", a1, []int{0, 1, 2}, []int{0, 1, 2, 3, 4}},
		{"Q2", "泵A", a1, []int{0, 1, 2}, []int{0, 1, 2, 3, 4}},
		{"Q3", "调节阀B", a1, []int{0, 1, 2, 4}, []int{0, 1, 2, 4, 5}},
		{"Q4", specialName, a1, []int{4, 1}, []int{5}},
		{"Q5", "孤立标签", a1, nil, nil}, {"Q6", "不存在", a1, nil, nil},
		{"Q7", "主泵A", a2, []int{5, 6}, []int{6}}, {"Q8", "主泵A", b1, []int{7, 8}, []int{7}},
		{"Q9", "主泵A", types.NameSpace{KnowledgeBase: "kb-a"}, []int{0, 1, 2, 5, 6}, []int{0, 1, 2, 3, 4, 6}},
		{"Q10", "主泵a", a1, nil, nil},
	}
	checks := []fixtureCheck{}
	for _, q := range queries {
		c := newCheck(q.id, "SearchNode", "baseline", oracleState(q.nodes, q.edges, len(q.nodes)))
		c.Namespace, c.QueryNames, c.NonNil = &q.ns, []string{q.name}, true
		switch q.id {
		case "Q7":
			c.ForbiddenNamespaces = []types.NameSpace{a1, b1}
			c.ForbiddenChunks = []string{"c-a1-01", "c-a1-02", "c-a1-03", "c-a1-04", "c-a1-special", "c-a1-05", "c-a1-06", "c-b1-01"}
		case "Q8":
			c.ForbiddenNamespaces = []types.NameSpace{a1, a2}
			c.ForbiddenChunks = []string{"c-a1-01", "c-a1-02", "c-a1-03", "c-a1-04", "c-a1-special", "c-a1-05", "c-a1-06", "c-a2-01"}
		case "Q9":
			c.ForbiddenNamespaces = []types.NameSpace{b1}
			c.ForbiddenChunks = []string{"c-b1-01", "c-a1-05", "c-a1-06"}
		default:
			c.ForbiddenNamespaces = []types.NameSpace{a2, b1}
			c.ForbiddenChunks = []string{"c-a2-01", "c-b1-01", "c-a1-05", "c-a1-06"}
		}
		if q.id == "Q1" {
			k := entityKey{"kb-a", "doc-a1", "主泵A"}
			c.First = &k
		}
		checks = append(checks, c)
	}
	checks = append(checks, newCheck("S1", "InspectSources", "baseline", oracleState([]int{0}, nil, 0)))
	r := newCheck("R1", "RepeatBaseline", "baseline", baselineOracle())
	r.PreservedNamespaces = []types.NameSpace{a1, a2, b1}
	r.ForbiddenChunks = []string{"c-a1-05", "c-a1-06"}
	c := newCheck("C1", "AppendConcurrently", "R1", baselineOracle())
	c.InputRefs = []string{"A1-concurrent-05", "A1-concurrent-06"}
	c.PreservedNamespaces = []types.NameSpace{a2, b1}
	c.Expected.Nodes[0].Chunks = []string{"c-a1-01", "c-a1-02", "c-a1-05", "c-a1-06"}
	d1 := newCheck("D1", "DelGraph", "C1", oracleState([]int{5, 6, 7, 8}, []int{6, 7}, 4))
	d1.DeleteNamespaces = []types.NameSpace{a1, a1}
	d1.PreservedNamespaces = []types.NameSpace{a2, b1}
	d1.ForbiddenNamespaces = []types.NameSpace{a1}
	d1.ForbiddenChunks = []string{"c-a1-01", "c-a1-02", "c-a1-03", "c-a1-04", "c-a1-special", "c-a1-05", "c-a1-06"}
	d1.EmptyQueries = []string{"Q1", "Q2", "Q3", "Q4", "Q5", "Q6"}
	d1.NoSourceResidue = true
	d2 := newCheck("D2", "DelGraph", "D1", oracleState(nil, nil, 0))
	d2.DeleteNamespaces = []types.NameSpace{a2, b1}
	d2.ForbiddenNamespaces = []types.NameSpace{a1, a2, b1}
	d2.NoSourceResidue = true
	return append(checks, r, c, d1, d2)
}

func equalSet[T any](label string, got, want []T) error {
	key := func(v T) string { b, _ := json.Marshal(v); return string(b) }
	actual := map[string]bool{}
	for _, v := range got {
		k := key(v)
		if actual[k] {
			return fmt.Errorf("%s duplicate %s", label, k)
		}
		actual[k] = true
	}
	if len(got) != len(want) {
		return fmt.Errorf("%s count %d, want %d", label, len(got), len(want))
	}
	for _, v := range want {
		if !actual[key(v)] {
			return fmt.Errorf("%s missing %s", label, key(v))
		}
	}
	return nil
}
func compareState(got, want fixtureState) error {
	// The golden format requires explicit collections even for empty results.
	// This says nothing about nil versus empty slices in runtime GraphData.
	if got.Entities == nil || got.Relations == nil || got.Nodes == nil {
		return fmt.Errorf("missing required golden collection (entity_keys, relation_keys or nodes)")
	}
	if got.Connected != want.Connected {
		return fmt.Errorf("connected count %d, want %d", got.Connected, want.Connected)
	}
	if err := equalSet("entities", got.Entities, want.Entities); err != nil {
		return err
	}
	if err := equalSet("relations", got.Relations, want.Relations); err != nil {
		return err
	}
	// Chunk order has no contract meaning; duplicates do. Attributes remain an array.
	normalize := func(nodes []fixtureNode) ([]fixtureNode, error) {
		out := slices.Clone(nodes)
		for i, n := range out {
			chunks := slices.Clone(n.Chunks)
			slices.Sort(chunks)
			if len(slices.Compact(slices.Clone(chunks))) != len(chunks) {
				return nil, fmt.Errorf("duplicate chunks at %+v", n.Key)
			}
			out[i].Chunks = chunks
		}
		return out, nil
	}
	g, err := normalize(got.Nodes)
	if err != nil {
		return err
	}
	w, err := normalize(want.Nodes)
	if err != nil {
		return err
	}
	return equalSet("node properties", g, w)
}

func validateFixtures(in fixtureInput, out fixtureExpected) error {
	if !reflect.DeepEqual(in, inputOracle()) {
		return fmt.Errorf("input differs from fixed NG-01A calls/constants (including required metadata)")
	}
	if out.SchemaVersion != 1 || out.DatabaseStatus != "NOT_RUN" {
		return fmt.Errorf("invalid expected schema/evidence status")
	}
	if err := compareState(out.Baseline, baselineOracle()); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	want := checkOracle()
	if len(out.Checks) != 15 {
		return fmt.Errorf("expected 15 checks, got %d", len(out.Checks))
	}
	seen := map[string]bool{}
	for _, g := range out.Checks {
		if seen[g.ID] {
			return fmt.Errorf("duplicate check %q", g.ID)
		}
		seen[g.ID] = true
		idx := slices.IndexFunc(want, func(c fixtureCheck) bool { return c.ID == g.ID })
		if idx < 0 {
			return fmt.Errorf("unknown check %q", g.ID)
		}
		w := want[idx]
		if err := compareState(g.Expected, w.Expected); err != nil {
			return fmt.Errorf("%s: %w", g.ID, err)
		}
		g.Expected, w.Expected = fixtureState{}, fixtureState{}
		if !reflect.DeepEqual(g, w) {
			return fmt.Errorf("%s: incorrect operation, precondition, scope, forbidden data or ordering metadata", g.ID)
		}
	}
	return nil
}

func TestContractFixtureFixedInputsAndGoldenSets(t *testing.T) {
	in, out := loadFixtures(t)
	if err := validateFixtures(in, out); err != nil {
		t.Fatal(err)
	}
	// Independent input completeness/closure census only; this does not execute a query.
	nodes := map[entityKey]bool{}
	edges := map[relationKey]bool{}
	connected := map[entityKey]bool{}
	for _, c := range in.Cases {
		for _, g := range c.Graphs {
			for _, n := range g.Node {
				nodes[entityKey{c.Namespace.KnowledgeBase, c.Namespace.Knowledge, n.Name}] = true
			}
			for _, r := range g.Relation {
				edges[relationKey{c.Namespace.KnowledgeBase, c.Namespace.Knowledge, r.Node1, r.Node2, r.Type}] = true
			}
		}
	}
	for e := range edges {
		for _, name := range []string{e.Node1, e.Node2} {
			k := entityKey{e.KB, e.Doc, name}
			if !nodes[k] {
				t.Fatalf("endpoint outside namespace: %+v", e)
			}
			connected[k] = true
		}
	}
	if len(nodes) != 9 || len(edges) != 8 || len(connected) != 8 {
		t.Fatalf("input census = %d/%d/%d, want 9/8/8", len(nodes), len(edges), len(connected))
	}
	// Golden array order is not a backend neighbour-order requirement. Q1's
	// separate raw-first-node assertion must survive set normalization.
	_, reordered := loadFixtures(t)
	slices.Reverse(reordered.Checks[0].Expected.Entities)
	slices.Reverse(reordered.Checks[0].Expected.Relations)
	slices.Reverse(reordered.Checks[0].Expected.Nodes)
	for i := range reordered.Checks[0].Expected.Nodes {
		slices.Reverse(reordered.Checks[0].Expected.Nodes[i].Chunks)
	}
	if err := validateFixtures(in, reordered); err != nil {
		t.Fatalf("set reordering changed the contract: %v", err)
	}
	for _, c := range out.Checks {
		t.Run(c.ID, func(t *testing.T) {
			t.Logf("E1 golden integrity only: %d entities / %d directed relations; real DB NOT_RUN", len(c.Expected.Entities), len(c.Expected.Relations))
		})
	}
}

func TestContractFixtureRejectsCorruption(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fixtureInput, *fixtureExpected)
	}{
		{"missing schema", func(i *fixtureInput, o *fixtureExpected) { i.SchemaVersion = 0 }},
		{"missing empty entity keys", func(i *fixtureInput, o *fixtureExpected) { o.Checks[4].Expected.Entities = nil }},
		{"missing namespace", func(i *fixtureInput, o *fixtureExpected) { i.Cases[0].Namespace = types.NameSpace{} }},
		{"duplicate call ID", func(i *fixtureInput, o *fixtureExpected) { i.Supplemental[0].ID = i.Cases[0].ID }},
		{"concurrent source in baseline", func(i *fixtureInput, o *fixtureExpected) {
			i.Cases[0].Graphs[0].Node[0].Chunks = append(i.Cases[0].Graphs[0].Node[0].Chunks, "c-a1-05")
		}},
		{"duplicate check ID", func(i *fixtureInput, o *fixtureExpected) { o.Checks[1].ID = "Q1" }},
		{"wrong source", func(i *fixtureInput, o *fixtureExpected) { o.Checks[0].Expected.Nodes[0].Chunks = []string{"c-a2-01"} }},
		{"duplicate source", func(i *fixtureInput, o *fixtureExpected) {
			o.Checks[12].Expected.Nodes[0].Chunks = append(o.Checks[12].Expected.Nodes[0].Chunks, "c-a1-05")
		}},
		{"reversed feedback", func(i *fixtureInput, o *fixtureExpected) {
			o.Checks[0].Expected.Relations[2].Node1 = "主泵A"
			o.Checks[0].Expected.Relations[2].Node2 = "调节阀B"
		}},
		{"missing parallel type", func(i *fixtureInput, o *fixtureExpected) {
			o.Baseline.Relations = append(o.Baseline.Relations[:1], o.Baseline.Relations[2:]...)
		}},
		{"Q3 extra self loop", func(i *fixtureInput, o *fixtureExpected) {
			o.Checks[2].Expected.Relations = append(o.Checks[2].Expected.Relations, o.Baseline.Relations[3])
		}},
		{"escaped newline instead of newline", func(i *fixtureInput, o *fixtureExpected) {
			i.Constants.S = "泵\"Q\\X\\n第二行"
			i.Cases[0].Graphs[0].Node[4].Name = i.Constants.S
		}},
		{"changed attribute", func(i *fixtureInput, o *fixtureExpected) {
			o.Checks[3].Expected.Nodes[0].Attributes = []string{"sanitized"}
		}},
		{"lost Q1 raw first node", func(i *fixtureInput, o *fixtureExpected) { o.Checks[0].First = nil }},
		{"fixed neighbor order", func(i *fixtureInput, o *fixtureExpected) { o.Checks[0].NeighborOrder = "alphabetical" }},
		{"nil empty graph allowed", func(i *fixtureInput, o *fixtureExpected) { o.Checks[4].NonNil = false }},
		{"D1 collateral deletion", func(i *fixtureInput, o *fixtureExpected) {
			o.Checks[13].Expected.Nodes = o.Checks[13].Expected.Nodes[:2]
		}},
		{"D2 residual entity", func(i *fixtureInput, o *fixtureExpected) {
			o.Checks[14].Expected.Entities = append(o.Checks[14].Expected.Entities, o.Baseline.Entities[0])
		}},
		{"invented real PASS", func(i *fixtureInput, o *fixtureExpected) { o.DatabaseStatus = "PASS" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, out := loadFixtures(t)
			tc.mutate(&in, &out)
			if err := validateFixtures(in, out); err == nil {
				t.Fatal("corrupted fixture was accepted")
			}
		})
	}
	for _, raw := range []string{`{"schema_version":1,"unexpected":true}`, `{} {}`} {
		var in fixtureInput
		if strictDecode([]byte(raw), &in) == nil {
			t.Fatalf("accepted unknown/trailing data: %s", raw)
		}
	}
	// Unknown fields within existing DTOs must also be diagnosed.
	var in fixtureInput
	if strictDecode([]byte(`{"cases":[{"graphs":[{"node":[{"name":"x","chunkz":[]}]}]}]}`), &in) == nil {
		t.Fatal("accepted misspelled DTO field")
	}
}

func TestContractFixtureSpecialCharactersAndExtractionSerialization(t *testing.T) {
	in, _ := loadFixtures(t)
	g := in.Cases[0].Graphs[0]
	for _, n := range g.Node {
		n.ID = "private-node"
		n.KnowledgeID = "private-doc"
	}
	for _, r := range g.Relation {
		r.ID = "private-relation"
		r.SourceID = "private-source"
		r.TargetID = "private-target"
	}
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-")) {
		t.Fatal("retrieval identities leaked into extraction JSON")
	}
	var decoded types.GraphData
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Node[4].Name != specialName || decoded.Node[4].Attributes[0] != specialAttribute || decoded.Relation[5].Type != specialType {
		t.Fatal("special characters changed in GraphData round trip")
	}
	for _, n := range decoded.Node {
		if n.ID != "" || n.KnowledgeID != "" {
			t.Fatal("decoded private node identity")
		}
	}
	for _, r := range decoded.Relation {
		if r.ID != "" || r.SourceID != "" || r.TargetID != "" {
			t.Fatal("decoded private relation identity")
		}
	}
}
