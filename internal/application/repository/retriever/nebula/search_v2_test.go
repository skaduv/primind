package nebula

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/primind/primind/internal/types"
)

// Layout v2 search verifies every record it returns; a corrupted record must
// fail the search instead of being returned or silently dropped.
func TestSearchVerifiesReturnedRecords(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	gold := loadGolden(t)
	for _, corruption := range []string{
		"relation_identity", "relation_namespace", "missing_to_edge",
		"entity_name_key", "entity_missing", "source_identity", "seed_identity",
	} {
		t.Run(corruption, func(t *testing.T) {
			wire := goldenWire(t, cfg, gold.Baseline, ns)
			original := wire.fn
			wire.fn = func(ctx context.Context, s string) wireOutcome {
				out := original(ctx, s)
				rows := out.Table.Rows
				isRel := strings.Contains(s, "$^."+relationTag)
				isSrc := strings.Contains(s, "$$."+sourceTag)
				// relation rows: rel,kind,ent,rank, r_kb..r_kind (4..8), e_kb..e_attrs (9..13)
				// source rows:   src,ent, s_kb,s_doc,s_name,s_chunk (2..5)
				switch {
				case corruption == "relation_identity" && isRel && len(rows) > 0:
					rows[0][8] = "篡改的类型"
				case corruption == "relation_namespace" && isRel && len(rows) > 0:
					rows[0][4] = "kb-other"
				case corruption == "missing_to_edge" && isRel:
					var kept [][]string
					for _, r := range rows {
						if r[1] != "primind_to" {
							kept = append(kept, r)
						}
					}
					out.Table.Rows = kept
				case corruption == "entity_name_key" && isRel && len(rows) > 0:
					rows[0][12] = "something else"
				case corruption == "entity_missing" && isRel && len(rows) > 0:
					for i := 9; i < 14; i++ {
						rows[0][i] = ""
					}
				case corruption == "source_identity" && isSrc && len(rows) > 0:
					rows[0][5] = "c-forged"
				case corruption == "seed_identity" && strings.Contains(s, "AS name_key |") && len(rows) > 0:
					rows[0][0] = "ng2_forged"
				}
				return out
			}
			g, _, err := testRepo(cfg, wire).Search(context.Background(), ns, []string{"主泵"})
			requireErr(t, err, ErrInvalid)
			if g != nil {
				t.Fatal("corrupted search returned a graph")
			}
		})
	}
}

// Full-width, case and spacing variants of a name reach the same seeds.
func TestSearchMatchesNormalisedNames(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	gold := loadGolden(t)
	base, _, err := testRepo(cfg, goldenWire(t, cfg, gold.Baseline, ns)).Search(context.Background(), ns,
		[]string{"主泵A"})
	if err != nil || len(base.Relation) == 0 {
		t.Fatalf("baseline: %v", err)
	}
	for _, q := range []string{"主泵ａ", "主泵Ａ", " 主泵a "} {
		got, _, err := testRepo(cfg, goldenWire(t, cfg, gold.Baseline, ns)).Search(context.Background(), ns,
			[]string{q})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Relation) != len(base.Relation) {
			t.Fatalf("%q: %d relations, want %d", q, len(got.Relation), len(base.Relation))
		}
	}
	empty, _, err := testRepo(cfg, goldenWire(t, cfg, gold.Baseline, ns)).Search(context.Background(), ns,
		[]string{"   "})
	if err != nil || len(empty.Relation) != 0 {
		t.Fatalf("blank query: %v", err)
	}
}

// stateOf builds a wire baseline in ns from relation triples (node1, node2, type).
func stateOf(ns types.NameSpace, rels ...[3]string) goldenState {
	st := goldenState{}
	seen := map[string]bool{}
	node := func(name string) {
		if !seen[name] {
			seen[name] = true
			st.Nodes = append(st.Nodes, goldenNode{Key: goldenKey{KB: ns.KnowledgeBase, Doc: ns.Knowledge, Name: name}})
		}
	}
	for _, r := range rels {
		node(r[0])
		node(r[1])
		st.Relations = append(st.Relations, goldenRelation{
			KB: ns.KnowledgeBase, Doc: ns.Knowledge, Node1: r[0], Node2: r[1], Type: r[2],
		})
	}
	return st
}

// Substring matching is evaluated in the store, so it no longer depends on
// how many entities the KB holds: a mid-name query still matches in a KB
// larger than SnapshotRecords.
func TestSearchSubstringInKBLargerThanSnapshotRecords(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	st := stateOf(ns, [3]string{"主泵A-备用", "阀门", "连接"})
	for i := 0; i < cfg.SnapshotRecords+50; i++ {
		st.Nodes = append(st.Nodes, goldenNode{Key: goldenKey{
			KB: ns.KnowledgeBase, Doc: ns.Knowledge, Name: fmt.Sprintf("填充实体-%05d", i),
		}})
	}
	g, d, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Search(context.Background(), ns, []string{"备用"})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Relation) != 1 || d.IndexOnly || d.CandidateTruncated {
		t.Fatalf("mid-name match in a large KB: %d relations, %+v", len(g.Relation), d)
	}
}

// A common word that matches more names than the cap still returns results,
// reports which stage cut them, and never loses an exact match to the cut.
func TestSearchTruncatesCandidatesAndKeepsExactMatch(t *testing.T) {
	cfg := testConfig()
	cfg.SnapshotRecords = 3
	ns := testNS()
	st := stateOf(ns,
		[3]string{"泵组总成", "机架", "属于"},
		[3]string{"阀1", "管路", "连接"}, [3]string{"阀2", "管路", "连接"}, [3]string{"阀3", "管路", "连接"},
		[3]string{"阀4", "管路", "连接"}, [3]string{"阀5", "管路", "连接"},
	)
	// "阀" matches five short names; the in-store order (shortest first) would
	// push the longer exact match "泵组总成" past the cap of three.
	search := func() (*types.GraphData, SearchDiagnostics) {
		g, d, err := testRepo(cfg, goldenWire(t, cfg, st, ns)).Search(context.Background(), ns,
			[]string{"泵组总成", "阀"})
		if err != nil {
			t.Fatalf("truncated search must not fail: %v", err)
		}
		return g, d
	}
	g, d := search()
	if !d.CandidateTruncated || d.SeedTruncated || d.RelationTruncated || d.IndexOnly {
		t.Fatalf("truncation stage not reported: %+v", d)
	}
	names := map[string]bool{}
	for _, n := range g.Node {
		names[n.Name] = true
	}
	if !names["泵组总成"] || len(g.Relation) != 4 || g.Node[0].Name != "泵组总成" {
		t.Fatalf("exact match lost or not first: %v, %d relations", names, len(g.Relation))
	}
	// The cut is deterministic: the same query returns the same graph.
	again, _ := search()
	if !reflect.DeepEqual(canonicalGraph(g, ns.KnowledgeBase), canonicalGraph(again, ns.KnowledgeBase)) {
		t.Fatal("truncated result is not stable")
	}
}

// When the in-store substring filter cannot finish within SearchScanTimeout,
// search falls back to prefix matching through the name_key index and
// remembers the namespace, so later queries skip the slow scan.
func TestSearchFallsBackToPrefixWhenFilterTimesOut(t *testing.T) {
	cfg := testConfig()
	cfg.SearchScanTimeout = 20 * time.Millisecond
	ns := testNS()
	wire := goldenWire(t, cfg, loadGolden(t).Baseline, ns)
	read := wire.fn
	filters := 0
	wire.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.Contains(s, " CONTAINS ") {
			filters++
			<-ctx.Done() // the scan outlives its budget
			return wireOutcome{Sent: true}
		}
		return read(ctx, s)
	}
	r := testRepo(cfg, wire)
	g, d, err := r.Search(context.Background(), ns, []string{"主泵A"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.IndexOnly || len(g.Relation) == 0 {
		t.Fatalf("prefix fallback not used: %+v", d)
	}
	// Prefix semantics: "主泵A" also finds "主泵A-备用"; a mid-name query does not.
	names := map[string]bool{}
	for _, n := range g.Node {
		names[n.Name] = true
	}
	if !names["主泵A-备用"] {
		t.Fatal("prefix match missing")
	}
	g, d, err = r.Search(context.Background(), ns, []string{"备用"})
	if err != nil || !d.IndexOnly || len(g.Relation) != 0 {
		t.Fatalf("mid-name query in index mode: %v %+v", err, d)
	}
	if filters != 1 {
		t.Fatalf("slow filter ran %d times, want once", filters)
	}
}

// A failure of the filter that is not its own timeout is an error, not a
// silent downgrade to prefix matching.
func TestSearchFilterErrorIsNotAFallback(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	wire := goldenWire(t, cfg, loadGolden(t).Baseline, ns)
	read := wire.fn
	wire.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.Contains(s, " CONTAINS ") {
			return wireOutcome{Sent: true, Err: ErrInvalid}
		}
		return read(ctx, s)
	}
	g, d, err := testRepo(cfg, wire).Search(context.Background(), ns, []string{"主泵A"})
	requireErr(t, err, ErrInvalid)
	if g != nil || d.IndexOnly {
		t.Fatalf("filter error downgraded: %+v", d)
	}
}
