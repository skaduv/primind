package nebula

import (
	"context"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// realNameKeyWhitespace is TestV2RealLayout's name_key_whitespace subtest:
// the acceptance cases of the name_key whitespace rule (see CONTRACT.md) that
// need a real NebulaGraph: hits in both directions on every search path, no
// joining of separate words, whole-name priority, names stored verbatim, and
// records keyed by the old rule rejected on read. It uses its own KB and
// deletes it at the end, so later subtests see the baseline unchanged.
func realNameKeyWhitespace(ctx context.Context, t *testing.T, cfg Config, r *Repository) {
	const kb = "kb-space"
	doc := func(name string) types.NameSpace { return types.NameSpace{KnowledgeBase: kb, Knowledge: name} }
	whole := types.NameSpace{KnowledgeBase: kb}
	var written []types.NameSpace
	add := func(ns types.NameSpace, rels ...[3]string) {
		t.Helper()
		g := &types.GraphData{}
		for _, rel := range rels {
			g.Relation = append(g.Relation, &types.GraphRelation{Node1: rel[0], Node2: rel[1], Type: rel[2]})
		}
		if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
			t.Fatalf("AddGraph %v: %v", ns, err)
		}
		waitVisible(t, r, ns)
		written = append(written, ns)
	}
	// Index-only twin of r: the substring filter can never finish, so seeds
	// come from the STARTS WITH fallback (IndexOnly).
	prefixCfg := cfg
	prefixCfg.SearchScanTimeout = time.Nanosecond
	prefix, err := NewRepository(ctx, prefixCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prefix.Close(context.Background()) }()

	// Item 1: each spelling finds the other, on every path.
	for i, pair := range spacingPairs {
		for j, stored := range pair {
			query := pair[1-j]
			ns := doc("doc-pair-" + strconv.Itoa(i) + "-" + strconv.Itoa(j))
			add(ns, [3]string{stored, "部件", "供应"})
			key := nameKey(stored)
			if key != nameKey(query) {
				t.Fatalf("premise: %q and %q must share a key", stored, query)
			}
			// The two in-store seed statements, each on its own.
			scope := `primind_entity.kb == "` + kb + `" AND primind_entity.doc == "` + ns.Knowledge + `"`
			exact := columnValues(t, rawQuery(t, cfg, `LOOKUP ON primind_entity WHERE `+scope+
				` AND primind_entity.name_key IN ["`+key+`"] YIELD primind_entity.name AS name`), "name")
			contains := columnValues(t, rawQuery(t, cfg, `LOOKUP ON primind_entity WHERE `+scope+
				` YIELD primind_entity.name AS name, primind_entity.name_key AS k`+
				` | YIELD $-.name AS name WHERE $-.k CONTAINS "`+key+`"`), "name")
			if len(exact) != 1 || exact[0] != stored || len(contains) != 1 || contains[0] != stored {
				t.Fatalf("%q: exact %q, contains %q", stored, exact, contains)
			}
			for _, repo := range []*Repository{r, prefix} {
				if repo == prefix {
					forceIndexOnly(prefix, ns)
				}
				g, d, err := repo.Search(ctx, ns, []string{query})
				if err != nil || len(g.Relation) != 1 || g.Relation[0].Node1 != stored ||
					d.IndexOnly != (repo == prefix) {
					t.Fatalf("query %q -> stored %q (index only %v): %v %+v", query, stored, repo == prefix, err, d)
				}
			}
		}
	}

	// Item 2: no joined words.
	neg := doc("doc-negative")
	add(neg, [3]string{"the rock", "某演员", "别名"}, [3]string{"no table", "某餐厅", "状态"},
		[3]string{"数据 库存", "某仓库", "属于"})
	for _, q := range []string{"hero", "notable", "数据库"} {
		g, _, err := r.Search(ctx, neg, []string{q})
		if err != nil || len(g.Relation) != 0 {
			t.Fatalf("query %q matched %d relations: %v", q, len(g.Relation), err)
		}
	}

	// Item 3: both spellings in one document are two records with their own
	// names and IDs, both found as whole-name matches.
	both := doc("doc-both")
	add(both, [3]string{"苏州 rotor精工", "部件甲", "供应"}, [3]string{"苏州rotor精工", "部件乙", "供应"})
	for _, q := range []string{"苏州 rotor精工", "苏州rotor精工"} {
		g, _, err := r.Search(ctx, both, []string{q})
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]string{}
		for _, n := range g.Node {
			ids[n.Name] = n.ID
		}
		if ids["苏州 rotor精工"] == "" || ids["苏州rotor精工"] == "" || ids["苏州 rotor精工"] == ids["苏州rotor精工"] ||
			len(g.Relation) != 2 {
			t.Fatalf("query %q: %v, %d relations", q, ids, len(g.Relation))
		}
	}

	// Items 5 and 6: spaced names survive Snapshot verbatim.
	rt := doc("doc-roundtrip")
	names := []string{
		"苏州\trotor精工", "苏州　rotor精工", "HX-200  电驱平台", "the　rock", "type\t2 diabetes", "C++ 语言",
	}
	var rels [][3]string
	for _, n := range names {
		rels = append(rels, [3]string{n, "部件", "供应"})
	}
	add(rt, rels...)
	snap, err := r.Snapshot(ctx, rt)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range snap.Node {
		if n.Name != "部件" {
			got = append(got, n.Name)
		}
	}
	sort.Strings(got)
	want := append([]string(nil), names...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("roundtrip names %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("roundtrip names %q, want %q", got, want)
		}
	}

	// Item 7: a name_key computed by the old rule fails full verification in
	// Snapshot and DelGraph; restoring the current key heals it.
	old := doc("doc-pair-1-0") // stored "HX-200 电驱平台"
	id, _ := identity(cfg, "entity", old, "HX-200 电驱平台")
	setKey := func(k string) {
		rawQuery(t, cfg, `UPDATE VERTEX ON primind_entity "`+id+`" SET name_key = "`+k+`"`)
	}
	setKey("hx-200 电驱平台")
	_, err = r.Snapshot(ctx, old)
	requireErr(t, err, ErrInvalid)
	requireErr(t, r.DelGraph(ctx, []types.NameSpace{old}), ErrInvalid)
	setKey(nameKey("HX-200 电驱平台"))
	if _, err := r.Snapshot(ctx, old); err != nil {
		t.Fatalf("restored key still rejected: %v", err)
	}

	// The whole KB is consistent, then removed.
	if _, err := r.Snapshot(ctx, whole); err != nil {
		t.Fatal(err)
	}
	if err := r.DelGraph(ctx, written); err != nil {
		t.Fatal(err)
	}
}
