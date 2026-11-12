package nebula

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// realLongNamePrefix is TestV2RealLayout's long_name_prefix subtest (plan
// section 9, item 7): primind_entity_key indexes only the first 256 bytes of
// name_key. Names whose key is longer than that must still be matched exactly
// by prefix queries, whether the prefix is shorter or longer than 256 bytes,
// on the index-only path (STARTS WITH) and on the exact path (IN).
func realLongNamePrefix(ctx context.Context, t *testing.T, cfg Config, r *Repository) {
	ns := types.NameSpace{KnowledgeBase: "kb-long", Knowledge: "doc-long"}
	body := strings.Repeat("甲", 100) // 300 bytes: past the 256-byte index prefix
	a, b, short := "长名"+body+"-尾巴A", "长名"+body+"-尾巴B", "长名短"
	ka, kb := nameKey(a), nameKey(b)
	if len(ka) <= 256 || ka[:256] != kb[:256] {
		t.Fatalf("premise: keys must share a prefix longer than 256 bytes (%d)", len(ka))
	}
	g := &types.GraphData{}
	for _, n := range []string{a, b, short} {
		g.Relation = append(g.Relation, &types.GraphRelation{Node1: n, Node2: "部件", Type: "连接"})
	}
	if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
		t.Fatal(err)
	}
	waitVisible(t, r, ns)
	prefixCfg := cfg
	prefixCfg.SearchScanTimeout = time.Nanosecond
	prefix, err := NewRepository(ctx, prefixCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prefix.Close(context.Background()) }()

	long := ka[:len(ka)-len("-尾巴a")+len("-尾巴")] // 313 bytes, shared by a and b
	for _, c := range []struct {
		name, query string
		want        []string
	}{
		{"short prefix", "长名", []string{a, b, short}},
		{"prefix past 256 bytes shared by both", long, []string{a, b}},
		{"prefix past 256 bytes, only a", ka, []string{a}},
		{"prefix past 256 bytes, only b", kb, []string{b}},
	} {
		for _, repo := range []*Repository{prefix, r} {
			got, d, err := repo.Search(ctx, ns, []string{c.query})
			if err != nil || d.IndexOnly != (repo == prefix) {
				t.Fatalf("%s: %v %+v", c.name, err, d)
			}
			var names []string
			for _, rel := range got.Relation {
				names = append(names, rel.Node1)
			}
			sort.Strings(names)
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if strings.Join(names, "\n") != strings.Join(want, "\n") {
				t.Fatalf("%s (index only %v): %d seeds, want %d", c.name, repo == prefix, len(names), len(want))
			}
		}
		// What the engine itself returns for the STARTS WITH statement,
		// before the adapter's own re-check of each row.
		raw := columnValues(t, rawQuery(t, cfg, `LOOKUP ON primind_entity WHERE primind_entity.kb == "kb-long"`+
			` AND primind_entity.name_key STARTS WITH "`+c.query+`" YIELD primind_entity.name AS name`), "name")
		if len(raw) != len(c.want) {
			t.Fatalf("%s: engine STARTS WITH returned %d rows, want %d", c.name, len(raw), len(c.want))
		}
		t.Logf("%s: query %d bytes, engine rows %d, adapter seeds %d", c.name, len(c.query), len(raw), len(c.want))
	}
	// Exact path: name_key IN [...] with a key past 256 bytes.
	raw := columnValues(t, rawQuery(t, cfg, `LOOKUP ON primind_entity WHERE primind_entity.kb == "kb-long"`+
		` AND primind_entity.name_key IN ["`+ka+`"] YIELD primind_entity.name AS name`), "name")
	if len(raw) != 1 || raw[0] != a {
		t.Fatalf("exact IN on a long key: %d rows", len(raw))
	}
	if err := r.DelGraph(ctx, []types.NameSpace{ns}); err != nil {
		t.Fatal(err)
	}
}
