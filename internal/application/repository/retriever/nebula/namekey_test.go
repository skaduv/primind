package nebula

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// The name_key rule (see CONTRACT.md, primind_entity): NFKC,
// lower case, split on white space, then rejoin directly where one side of the
// boundary is Han and the other a-z/0-9, and with one space everywhere else.
// Each case lists the input and the exact expected key.
func TestNameKeyFixedCases(t *testing.T) {
	cases := []struct{ in, want string }{
		// Section 9.2, first table.
		{"苏州 rotor精工", "苏州rotor精工"},
		{"HX-200 电驱平台", "hx-200电驱平台"},
		{"O'Neil 陈", "o'neil陈"},
		{"type ２  diabetes mellitus", "type 2 diabetes mellitus"},
		{"the rock", "the rock"},
		{"数据 库存", "数据 库存"},
		{"C++ 语言", "c++ 语言"},
		// Either order, several boundaries, any white space run.
		{"rotor 精工", "rotor精工"},
		{"精工 rotor", "精工rotor"},
		{"a 中 b", "a中b"},
		{"中 a 中", "中a中"},
		{"苏州\t\n rotor精工", "苏州rotor精工"},
		{"苏州　rotor精工", "苏州rotor精工"},
		{"  苏州 rotor精工  ", "苏州rotor精工"},
		// Full-width and upper case are folded before the boundary check.
		{"苏州 Ｒｏｔｏｒ精工", "苏州rotor精工"},
		{"ＨＸ-２００ 电驱平台", "hx-200电驱平台"},
		{"苏州 ROTOR", "苏州rotor"},
		// Unchanged cases kept from the original rule.
		{"Dexamethasone", "dexamethasone"},
		{"ｄｅｘａｍｅｔｈａｓｏｎｅ", "dexamethasone"},
		{"type ２  diabetes\tmellitus ", "type 2 diabetes mellitus"},
		{"地塞米松", "地塞米松"},
		{"　全角空格　", "全角空格"},
		{"张 三", "张 三"},
		{"Space X", "space x"},
		{"C++ 语言 2", "c++ 语言2"},
		{"", ""},
		{" \t　", ""},
	}
	// Han range edges (section 9.2): the first of each pair is outside, the
	// second inside, or the reverse for upper bounds. "a" is the other side.
	for _, c := range []struct {
		r   rune
		han bool
	}{
		{0x33FF, false},
		{0x3400, true},
		{0x4DBF, true},
		{0x4DC0, false},
		{0x4DFF, false},
		{0x4E00, true},
		{0x9FFF, true},
		{0xA000, false},
		{0x1FFFF, false},
		{0x20000, true},
		{0x3FFFF, true},
		{0x40000, false},
		{0x3007, true},
		{0x3006, false},
	} {
		in := string(c.r) + " a"
		// NFKC may rewrite a compatibility character (U+33FF is one); the key
		// is built from the normalised form, so compute the expected left side
		// the same way the rule does.
		left := nameKey(string(c.r))
		want := left + " a"
		if c.han {
			want = left + "a"
		}
		cases = append(cases, struct{ in, want string }{in, want})
		if c.han {
			cases = append(cases, struct{ in, want string }{"7 " + string(c.r), "7" + left})
		}
	}
	// a-z / 0-9 range edges next to a Han character.
	for _, c := range []struct {
		s     string
		alnum bool
	}{
		{"/", false},
		{"0", true},
		{"9", true},
		{":", false},
		{"`", false},
		{"a", true},
		{"z", true},
		{"{", false},
		{"é", false},
		{"か", false},
		{"한", false},
		{"α", false},
		{"ж", false},
	} {
		want := c.s + " 中"
		if c.alnum {
			want = c.s + "中"
		}
		cases = append(cases, struct{ in, want string }{c.s + " 中", want})
	}
	for _, c := range cases {
		if got := nameKey(c.in); got != c.want {
			t.Errorf("nameKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// U+33FF is outside the Han ranges; pin what NFKC turns it into so the
	// boundary case above stays meaningful.
	if got := nameKey("㏿"); got != "gal" {
		t.Errorf("nameKey(U+33FF) = %q, want %q", got, "gal")
	}
	if !isHan(0x3007) || isHan(0x3006) || isLatinAlnum('A') || !isLatinAlnum('z') {
		t.Error("character class edges")
	}
}

// nameKey is idempotent, and a name made only of white space has an empty key.
func TestNameKeyProperties(t *testing.T) {
	for _, in := range []string{
		"苏州 rotor精工", "HX-200 电驱平台", "O'Neil 陈", "type ２  diabetes mellitus", "数据 库存",
		"C++ 语言", "a 中 b 中 c", "ＨＸ-２００　电驱平台", "é 中", "か 中", "한 中", "〇 1", "\U00020000 x",
		"ﬁ 中", "Ⅻ 号", "① 号", "İstanbul 站", "ß 中", "主泵A-备用", "  ", "\t\n　",
	} {
		k := nameKey(in)
		if again := nameKey(k); again != k {
			t.Errorf("nameKey not idempotent for %q: %q then %q", in, k, again)
		}
	}
	for _, blank := range []string{"", " ", "\t\n", "　", " 　\t"} {
		if k := nameKey(blank); k != "" {
			t.Errorf("blank name %q has key %q", blank, k)
		}
	}
}

// spacingPairs are the section 9.4 pairs: each spelling must find the other.
var spacingPairs = [][2]string{
	{"苏州 rotor精工", "苏州rotor精工"},
	{"HX-200 电驱平台", "HX-200电驱平台"},
	{"O'Neil 陈", "O'Neil陈"},
}

// pathWire answers like goldenWire but forces search through one seed path:
// "exact" drops the substring filter's rows, "contains" drops the exact
// lookup's rows, "prefix" makes the filter time out and drops exact rows so
// the seeds can only come from the STARTS WITH fallback.
func pathWire(t *testing.T, cfg Config, st goldenState, ns types.NameSpace, path string) *testWire {
	t.Helper()
	w := goldenWire(t, cfg, st, ns)
	read := w.fn
	w.fn = func(ctx context.Context, s string) wireOutcome {
		filter := strings.Contains(s, " CONTAINS ")
		exact := strings.Contains(s, ".name_key IN [")
		if path == "prefix" && filter {
			<-ctx.Done()
			return wireOutcome{Sent: true}
		}
		out := read(ctx, s)
		if (path == "exact" && filter) || (path != "exact" && exact) {
			out.Table.Rows = nil
		}
		return out
	}
	return w
}

// Section 9.4 item 1: a spelling with or without the space between Han and
// Latin/digits finds the other, on each of the three seed paths.
func TestSearchSpacingVariantsOnEveryPath(t *testing.T) {
	ns := testNS()
	for _, path := range []string{"exact", "contains", "prefix"} {
		for _, pair := range spacingPairs {
			for _, dir := range [][2]string{{pair[0], pair[1]}, {pair[1], pair[0]}} {
				stored, query := dir[0], dir[1]
				cfg := testConfig()
				cfg.SearchScanTimeout = 20 * time.Millisecond
				st := stateOf(ns, [3]string{stored, "某部件", "供应"})
				g, d, err := testRepo(cfg, pathWire(t, cfg, st, ns, path)).Search(context.Background(), ns,
					[]string{query})
				if err != nil {
					t.Fatalf("%s %q -> %q: %v", path, query, stored, err)
				}
				if len(g.Relation) != 1 || g.Relation[0].Node1 != stored || d.IndexOnly != (path == "prefix") {
					t.Fatalf("%s: query %q did not find stored %q: %d relations %+v", path, query, stored,
						len(g.Relation), d)
				}
			}
		}
	}
}

// Section 9.4 item 2: removing spaces only at Han/Latin boundaries does not
// make Latin words or Han words run together.
func TestSearchSpacingDoesNotJoinWords(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	st := stateOf(ns,
		[3]string{"the rock", "某演员", "别名"},
		[3]string{"no table", "某餐厅", "状态"},
		[3]string{"数据 库存", "某仓库", "属于"},
	)
	for _, q := range []string{"hero", "notable", "数据库"} {
		g, _, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Search(context.Background(), ns, []string{q})
		if err != nil || len(g.Relation) != 0 {
			t.Fatalf("query %q matched %d relations (%v)", q, len(g.Relation), err)
		}
	}
}

// Section 9.4 item 3: both spellings in one document are separate records
// (own names, own IDs) and both are whole-name matches, ranked ahead of the
// shorter names that only contain the other query word (the plain ranking
// would put shorter names first). Which spelling comes first is not asserted.
func TestSearchSpacingVariantsAreWholeNameMatches(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	for _, pair := range spacingPairs {
		st := stateOf(ns,
			[3]string{pair[0], "部件甲", "供应"},
			[3]string{pair[1], "部件乙", "供应"},
		)
		// Short names that only contain the second query word: the in-store
		// filter orders them first by length.
		for _, short := range []string{"x1", "x2", "x3"} {
			st.Relations = append(st.Relations, goldenRelation{
				KB: ns.KnowledgeBase, Doc: ns.Knowledge, Node1: short, Node2: "部件丙", Type: "连接",
			})
			st.Nodes = append(st.Nodes, goldenNode{
				Key: goldenKey{KB: ns.KnowledgeBase, Doc: ns.Knowledge, Name: short},
			})
		}
		st.Nodes = append(st.Nodes, goldenNode{Key: goldenKey{KB: ns.KnowledgeBase, Doc: ns.Knowledge, Name: "部件丙"}})
		for _, q := range pair {
			g, d, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Search(context.Background(), ns, []string{q, "x"})
			if err != nil || d.CandidateTruncated {
				t.Fatalf("%q: %v %+v", q, err, d)
			}
			ids := map[string]string{}
			pos := map[string]int{}
			for i, n := range g.Node {
				ids[n.Name] = n.ID
				pos[n.Name] = i
			}
			if ids[pair[0]] == "" || ids[pair[1]] == "" || ids[pair[0]] == ids[pair[1]] {
				t.Fatalf("%q: both spellings must be kept verbatim with distinct IDs: %v", q, ids)
			}
			for _, short := range []string{"x1", "x2", "x3"} {
				if ids[short] == "" || pos[short] < pos[pair[0]] || pos[short] < pos[pair[1]] {
					t.Fatalf("%q: substring-only %q not ranked after the whole-name matches: %v", q, short, pos)
				}
			}
			if len(g.Relation) != 5 {
				t.Fatalf("%q: %d relations, want 5", q, len(g.Relation))
			}
		}
	}
}

// Section 9.4 item 4: case and full-width variants still match, and runs of
// white space between Latin words still collapse to one space.
func TestSearchExistingNormalisationUnchanged(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	st := stateOf(ns,
		[3]string{"HX-200 电驱平台", "部件", "包含"},
		[3]string{"type 2 diabetes mellitus", "胰岛素", "治疗"},
	)
	for q, want := range map[string]string{
		"ＨＸ-２００电驱平台":                   "HX-200 电驱平台",
		"hx-200 电驱平台":                  "HX-200 电驱平台",
		"TYPE  ２\tDiabetes   Mellitus": "type 2 diabetes mellitus",
	} {
		g, _, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Search(context.Background(), ns, []string{q})
		if err != nil || len(g.Relation) != 1 || g.Relation[0].Node1 != want {
			t.Fatalf("query %q: %v %d relations", q, err, len(g.Relation))
		}
	}
	g, _, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Search(context.Background(), ns, []string{"type2"})
	if err != nil || len(g.Relation) != 0 {
		t.Fatalf("Latin words joined: %v %d relations", err, len(g.Relation))
	}
}

// withChunks gives every node one source chunk, which Snapshot and DelGraph
// read through owner edges.
func withChunks(st goldenState) goldenState {
	for i := range st.Nodes {
		st.Nodes[i].Chunks = []string{"c-" + st.Nodes[i].Key.Name}
	}
	return st
}

// Section 9.4 items 5 and 6: names with spaces, tabs and ideographic spaces
// (at Han/Latin boundaries and elsewhere) pass Snapshot verification and come
// back exactly as written.
func TestSnapshotKeepsSpacedNamesVerbatim(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	names := []string{
		"苏州 rotor精工", "苏州\trotor精工", "苏州　rotor精工", "O'Neil  陈",
		"数据 库存", "the　rock", "type\t2 diabetes", "C++ 语言",
	}
	var rels [][3]string
	for _, n := range names {
		rels = append(rels, [3]string{n, "部件", "供应"})
	}
	g, err := testRepo(cfg, goldenWire(t, cfg, withChunks(stateOf(ns, rels...)), ns)).Snapshot(context.Background(), ns)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range g.Node {
		got[n.Name] = true
	}
	for _, n := range names {
		if !got[n] {
			t.Errorf("name %q not returned verbatim", n)
		}
	}
}

// Section 9.4 item 7: a record whose stored name_key was computed by the old
// rule ("hx-200 电驱平台") fails full verification, in Snapshot and in
// DelGraph, which reads the namespace the same way before deleting anything.
func TestOldRuleNameKeyFailsVerification(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	st := withChunks(stateOf(ns, [3]string{"HX-200 电驱平台", "部件", "包含"}))
	oldKey := strings.Join(strings.Fields(strings.ToLower("HX-200 电驱平台")), " ")
	if oldKey == nameKey("HX-200 电驱平台") {
		t.Fatal("test premise: old and new keys must differ")
	}
	wire := func() *testWire {
		w := goldenWire(t, cfg, st, ns)
		read := w.fn
		w.fn = func(ctx context.Context, s string) wireOutcome {
			out := read(ctx, s)
			if strings.HasPrefix(s, "DELETE ") {
				t.Error("delete issued for a namespace that failed verification")
			}
			if !strings.HasPrefix(s, "LOOKUP ON "+entityTag+" ") {
				return out
			}
			name, key := fieldIndex(out.Table, "name"), fieldIndex(out.Table, "name_key")
			for _, row := range out.Table.Rows {
				if name >= 0 && key >= 0 && row[name] == "HX-200 电驱平台" {
					row[key] = oldKey
				}
			}
			return out
		}
		return w
	}
	_, err := testRepo(cfg, wire()).Snapshot(context.Background(), ns)
	requireErr(t, err, ErrInvalid)
	err = testRepo(cfg, wire()).DelGraph(context.Background(), []types.NameSpace{ns})
	requireErr(t, err, ErrInvalid)
	// The same data with the key the current rule computes is healthy.
	if _, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Snapshot(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
}
