package nebula

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// realPrefixRange is TestV2RealLayout's prefix_range subtest: the prefix
// fallback's statement (index range plus STARTS WITH in the pipe, built by
// prefixSeedStatement) must return exactly the rows, in the same order, as a
// plain STARTS WITH lookup with the same sort and LIMIT, on the real engine.
// The prefixes cover code point boundaries where UTF-8 changes length, the
// surrogate gap, U+10FFFF, 4-byte characters, quotes and backslashes, keys
// and prefixes around and past the 256-byte index length, no hits, and more
// hits than LIMIT. It also searches each prefix through the adapter on the
// index-only path.
func realPrefixRange(ctx context.Context, t *testing.T, cfg Config, r *Repository) {
	u := func(rs ...rune) string { return string(rs) }
	k := func(n int) string { return strings.Repeat("k", n) }
	long := "长名" + strings.Repeat("甲", 100)
	names := []string{
		"ab", "abc", "abd", "ab~", u('a', 'b', 0x7f), u('a', 'b', 0x7f, 'z'), u('a', 'b', 0x80), "ac",
		u('x', 0x7ff), u('x', 0x7ff, 'z'), u('x', 0x800),
		u('y', 0xffff), u('y', 0xffff, 'z'), u('y', 0x10000),
		u('w', 0xd7ff), u('w', 0xd7ff, 'z'), u('w', 0xe000),
		u('v', 0x20000), u('v', 0x20000, 'z'), u('v', 0x20001), u('v', 0x10ffff), u('v', 0x10ffff, 'z'), "vz",
		u(0x10ffff, 0x10ffff), "汉字", "汉字甲", u('汉', 0x5b58),
		`q"x`, `q\x`, `q"y`, `q\\y`,
		long + "-尾巴a", long + "-尾巴b", "长名短",
		k(255) + "a", k(255) + "ab", k(255) + "b", k(255) + "aa" + strings.Repeat("z", 20), k(255) + u(0xe9, 'x'),
		k(255) + u(0xea, 'x'), k(254) + u(0xe9), k(254) + u(0xe9, 'a'), k(255) + u(0x7f, 'x'), k(255) + u(0x80, 'x'),
	}
	for i := 0; i < 30; i++ {
		names = append(names, fmt.Sprintf("多-%02d", i))
	}
	docs := []types.NameSpace{
		{KnowledgeBase: "kb-range", Knowledge: "doc-range-1"},
		{KnowledgeBase: "kb-range", Knowledge: "doc-range-2"},
	}
	var keys []string
	for _, n := range names {
		if nameKey(n) != n {
			t.Fatalf("premise: %q must be its own key, got %q", n, nameKey(n))
		}
		keys = append(keys, n)
	}
	for di, ns := range docs {
		g := &types.GraphData{}
		for i, n := range names {
			if di == 1 && i%2 == 1 {
				continue // the second document holds every other name
			}
			g.Relation = append(g.Relation, &types.GraphRelation{Node1: n, Node2: "部件", Type: "连接"})
		}
		if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
			t.Fatal(err)
		}
		waitVisible(t, r, ns)
	}
	prefixCfg := cfg
	prefixCfg.SearchScanTimeout = time.Nanosecond
	prefix, err := NewRepository(ctx, prefixCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = prefix.Close(context.Background()) }()

	pool, s := rawSession(t, cfg)
	defer pool.Close()
	defer s.Release()
	if rs, err := s.Execute("USE " + cfg.Space); err != nil || !rs.IsSucceed() {
		t.Fatalf("USE: %v", err)
	}
	run := func(stmt string, params map[string]string) []string {
		t.Helper()
		values := map[string]any{}
		for name, v := range params {
			values[name] = v
		}
		rs, err := s.ExecuteWithParameter(stmt, values)
		if err != nil || !rs.IsSucceed() {
			t.Fatalf("%s: %v %s", stmt, err, rs.GetErrorMsg())
		}
		var rows []string
		for i := 0; i < rs.GetRowSize(); i++ {
			rec, err := rs.GetRowValuesByIndex(i)
			if err != nil {
				t.Fatal(err)
			}
			var cols []string
			for _, c := range candidateColumns {
				v, err := rec.GetValueByColName(c)
				if err != nil {
					t.Fatal(err)
				}
				str, _ := v.AsString()
				cols = append(cols, str)
			}
			rows = append(rows, strings.Join(cols, "|"))
		}
		return rows
	}

	prefixes := []string{
		"a", "ab", "abc", "zz-none", "多-", "多-0", "多-29",
		u('a', 'b', 0x7f), u('x', 0x7ff), u('y', 0xffff), u('w', 0xd7ff), u('v', 0x20000), u('v', 0x10ffff),
		u(0x10ffff), u(0x10ffff, 0x10ffff), "汉字", "汉", `q"`, `q\`, `q"x`,
		"长名", long + "-尾巴", long + "-尾巴a", long + "-尾巴b",
		k(254), k(255), k(255) + "a", k(255) + "aa", k(256), k(255) + u(0xe9), k(254) + u(0xe9), k(255) + u(0x7f),
	}
	scopes := []types.NameSpace{{KnowledgeBase: "kb-range"}, docs[0]}
	for _, scope := range scopes {
		for _, limit := range []int{10000, 5} {
			for _, p := range prefixes {
				where, params := scopeWhere(entityTag, scope)
				params["q0"] = p
				newStmt := prefixSeedStatement(where, 0, p, limit, params)
				oldStmt := "LOOKUP ON " + entityTag + " WHERE " + where + " AND " + entityTag +
					".name_key STARTS WITH $q0" + candidateYield() +
					" | ORDER BY $-.name_key,$-.doc,$-.vid | LIMIT " + fmt.Sprint(limit+1) + ";"
				got, want := run(newStmt, params), run(oldStmt, params)
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Fatalf("scope %v limit %d prefix %q (%d bytes): range %d rows, STARTS WITH %d rows\n%q\n%q",
						scope, limit, p, len(p), len(got), len(want), got, want)
				}
				// The STARTS WITH rows themselves are what Go expects.
				expect := 0
				for i, n := range keys {
					if strings.HasPrefix(n, p) {
						expect++
						if scope.Knowledge == "" && i%2 == 0 {
							expect++ // also in the second document
						}
					}
				}
				if min(expect, limit+1) != len(want) {
					t.Fatalf("scope %v prefix %q: STARTS WITH %d rows, Go expects %d", scope, p, len(want), expect)
				}
				if limit == 10000 && scope.Knowledge == "" {
					t.Logf("prefix %3d bytes: %2d rows, range and STARTS WITH identical", len(p), len(got))
				}
			}
		}
	}
	// Through the adapter, index-only path: seeds are exactly the names
	// starting with the query.
	for _, p := range prefixes {
		forceIndexOnly(prefix, scopes[0])
		g, d, err := prefix.Search(ctx, scopes[0], []string{p})
		if err != nil || !d.IndexOnly {
			t.Fatalf("search %q: %v %+v", p, err, d)
		}
		got := map[string]bool{}
		for _, rel := range g.Relation {
			got[rel.Node1] = true
		}
		var missing, extra []string
		for _, n := range keys {
			if strings.HasPrefix(n, p) != got[n] {
				if got[n] {
					extra = append(extra, n)
				} else {
					missing = append(missing, n)
				}
			}
		}
		sort.Strings(missing)
		if len(missing)+len(extra) > 0 || d.SeedTruncated {
			t.Fatalf("search %q: missing %q extra %q %+v", p, missing, extra, d)
		}
	}
	if err := r.DelGraph(ctx, docs); err != nil {
		t.Fatal(err)
	}
}
