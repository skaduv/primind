package nebula

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	ng "github.com/vesoft-inc/nebula-go/v3"

	"github.com/primind/primind/internal/types"
)

// TestV2RealLayout runs layout v2 end to end against a real NebulaGraph:
// schema creation, the fixed golden baseline, every fixed query, Snapshot,
// hostile strings, search past SnapshotRecords, delete ownership, name_key
// white space (namekey_real_test.go), long-name prefixes (longname_real_test.go),
// the prefix fallback's index range against STARTS WITH (prefixrange_real_test.go)
// and deletion.
// It only runs with NEBULA_IT_V2_SPACE set to a throw-away
// primind_ng_verify_* Space, which it drops and recreates.
//
//	NEBULA_IT_ADDRS=127.0.0.1:9669 NEBULA_IT_V2_SPACE=primind_ng_verify_v2 \
//	NEBULA_IT_USERNAME=root NEBULA_IT_PASSWORD=nebula go test -run TestV2RealLayout
func TestV2RealLayout(t *testing.T) {
	addr, space := os.Getenv("NEBULA_IT_ADDRS"), os.Getenv("NEBULA_IT_V2_SPACE")
	if addr == "" || space == "" {
		t.Skip("NEBULA_IT_ADDRS / NEBULA_IT_V2_SPACE not set")
	}
	if !strings.HasPrefix(space, "primind_ng_verify_") {
		t.Fatal("NEBULA_IT_V2_SPACE must be a throw-away primind_ng_verify_* Space")
	}
	cfg := DefaultConfig()
	cfg.Addresses = []string{addr}
	cfg.Space = space
	cfg.Username, cfg.Password = os.Getenv("NEBULA_IT_USERNAME"), os.Getenv("NEBULA_IT_PASSWORD")
	cfg.ReadyTimeout = 60 * time.Second
	recreateSpace(t, cfg)
	ctx := context.Background()
	auth := SchemaAuthorization{Space: space, CreateInEmptyDedicatedSpace: true}
	if err := InitializeSchema(ctx, cfg, auth); err != nil {
		t.Fatal(err)
	}
	r, err := NewRepository(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(context.Background()) }()

	gold := loadGolden(t)
	byNS := map[types.NameSpace]*types.GraphData{}
	graphFor := func(kb, doc string) *types.GraphData {
		ns := types.NameSpace{KnowledgeBase: kb, Knowledge: doc}
		if byNS[ns] == nil {
			byNS[ns] = &types.GraphData{}
		}
		return byNS[ns]
	}
	for _, n := range gold.Baseline.Nodes {
		g := graphFor(n.Key.KB, n.Key.Doc)
		g.Node = append(g.Node, &types.GraphNode{Name: n.Key.Name, Chunks: n.Chunks, Attributes: n.Attributes})
	}
	for _, e := range gold.Baseline.Relations {
		g := graphFor(e.KB, e.Doc)
		g.Relation = append(g.Relation, &types.GraphRelation{Node1: e.Node1, Node2: e.Node2, Type: e.Type})
	}
	for ns, g := range byNS {
		if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
			t.Fatalf("AddGraph %v: %v", ns, err)
		}
	}
	waitVisible(t, r, types.NameSpace{KnowledgeBase: "kb-a", Knowledge: "doc-a1"})

	t.Run("fixed_queries", func(t *testing.T) {
		for _, q := range gold.Checks {
			if !strings.HasPrefix(q.ID, "Q") {
				continue
			}
			if q.ID == "Q10" { // v2 matches on the normalised key, see TestFixedQueries…
				for _, q1 := range gold.Checks {
					if q1.ID == "Q1" && q1.Namespace == q.Namespace {
						q.Expected, q.First = q1.Expected, q1.First
					}
				}
			}
			got, err := r.SearchNode(ctx, q.Namespace, q.QueryNames)
			if err != nil {
				t.Fatalf("%s: %v", q.ID, err)
			}
			if !reflect.DeepEqual(canonicalGraph(got, q.Namespace.KnowledgeBase), canonicalGolden(q.Expected)) {
				t.Fatalf("%s mismatch\ngot  %v\nwant %v", q.ID, canonicalGraph(got, q.Namespace.KnowledgeBase),
					canonicalGolden(q.Expected))
			}
			if q.First != nil && (len(got.Node) == 0 || got.Node[0].Name != q.First.Name ||
				got.Node[0].KnowledgeID != q.First.Doc) {
				t.Fatalf("%s first seed", q.ID)
			}
		}
	})

	t.Run("snapshot_equals_baseline", func(t *testing.T) {
		for ns := range byNS {
			snap, err := r.Snapshot(ctx, ns)
			if err != nil {
				t.Fatal(err)
			}
			want := goldenState{}
			for _, n := range gold.Baseline.Nodes {
				if n.Key.KB == ns.KnowledgeBase && n.Key.Doc == ns.Knowledge {
					want.Nodes = append(want.Nodes, n)
				}
			}
			for _, e := range gold.Baseline.Relations {
				if e.KB == ns.KnowledgeBase && e.Doc == ns.Knowledge {
					want.Relations = append(want.Relations, e)
				}
			}
			if !reflect.DeepEqual(canonicalGraph(snap, ns.KnowledgeBase), canonicalGolden(want)) {
				t.Fatalf("%v snapshot mismatch", ns)
			}
		}
	})

	t.Run("plaintext_and_hostile_strings", func(t *testing.T) {
		ns := types.NameSpace{KnowledgeBase: "kb-hostile", Knowledge: "doc-\"x\"\\y"}
		hostile := "泵\"Q\\X\n第二行\x00尾\x01'); DROP SPACE " + space + "; ("
		g := &types.GraphData{
			Node: []*types.GraphNode{{Name: hostile, Chunks: []string{"c\x00\"1"}, Attributes: []string{"a\n\"\\"}}},
			Relation: []*types.GraphRelation{
				{Node1: hostile, Node2: "ｄｅｘａｍｅｔｈａｓｏｎｅ", Type: "t\"y\\pe"},
			},
		}
		if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
			t.Fatal(err)
		}
		waitVisible(t, r, ns)
		snap, err := r.Snapshot(ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, n := range snap.Node {
			if n.Name == hostile {
				found = reflect.DeepEqual(n.Chunks, []string{"c\x00\"1"}) &&
					reflect.DeepEqual(n.Attributes, []string{"a\n\"\\"})
			}
		}
		if !found || len(snap.Relation) != 1 || snap.Relation[0].Type != "t\"y\\pe" {
			t.Fatalf("hostile round trip failed: %+v", snap)
		}
		// Plaintext in the database: an operator's nGQL sees the real name.
		rs := rawQuery(t, cfg, `LOOKUP ON primind_entity WHERE primind_entity.kb == "kb-hostile" `+
			`YIELD primind_entity.name AS name, primind_entity.name_key AS k`)
		names := strings.Join(columnValues(t, rs, "name"), "|") + "|" + strings.Join(columnValues(t, rs, "k"), "|")
		if !strings.Contains(names, "ｄｅｘａｍｅｔｈａｓｏｎｅ") || !strings.Contains(names, "dexamethasone") {
			t.Fatalf("stored values are not plaintext / normalised: %q", names)
		}
		// Normalised search: half-width lower case finds the full-width entity.
		got, err := r.SearchNode(ctx, types.NameSpace{KnowledgeBase: "kb-hostile"}, []string{"Dexamethasone"})
		if err != nil || len(got.Relation) != 1 {
			t.Fatalf("normalised search: %v %+v", err, got)
		}
		if err := r.DelGraph(ctx, []types.NameSpace{ns}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("tamper_detected", func(t *testing.T) {
		ns := types.NameSpace{KnowledgeBase: "kb-a", Knowledge: "doc-a1"}
		rel, _ := identity(cfg, "relation", ns, "主泵A", "调节阀B", "连接")
		rawQuery(t, cfg, `UPDATE VERTEX ON primind_relation "`+rel+`" SET kind = "伪造"`)
		_, err := r.SearchNode(ctx, ns, []string{"主泵A"})
		requireErr(t, err, ErrInvalid)
		_, err = r.Snapshot(ctx, ns)
		requireErr(t, err, ErrInvalid)
		rawQuery(t, cfg, `UPDATE VERTEX ON primind_relation "`+rel+`" SET kind = "连接"`)
		if _, err := r.SearchNode(ctx, ns, []string{"主泵A"}); err != nil {
			t.Fatalf("restored record still rejected: %v", err)
		}
	})

	t.Run("substring_and_truncation_beyond_snapshot_records", func(t *testing.T) {
		// 12000 entities in one KB: more than SnapshotRecords (10000).
		const kb, docs, perDoc = "kb-big", 3, 4000
		var all []types.NameSpace
		for d := 0; d < docs; d++ {
			ns := types.NameSpace{KnowledgeBase: kb, Knowledge: fmt.Sprintf("doc-big-%d", d)}
			all = append(all, ns)
			g := &types.GraphData{}
			hub := fmt.Sprintf("总管-%d", d)
			for i := 0; i < perDoc; i++ {
				name := fmt.Sprintf("阀-%d-%04d", d, i)
				g.Node = append(g.Node, &types.GraphNode{Name: name})
				if i < 300 {
					g.Relation = append(g.Relation, &types.GraphRelation{Node1: name, Node2: hub, Type: "连接"})
				}
			}
			if d == 0 {
				g.Relation = append(g.Relation, &types.GraphRelation{Node1: "甲-独特中段-乙", Node2: hub, Type: "连接"})
			}
			if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
				t.Fatalf("AddGraph %v: %v", ns, err)
			}
			waitVisible(t, r, ns)
		}
		whole := types.NameSpace{KnowledgeBase: kb}
		// A mid-name query still matches although the KB is past the old limit.
		start := time.Now()
		g, d, err := r.Search(ctx, whole, []string{"独特中段"})
		if err != nil || len(g.Relation) != 1 || d.IndexOnly || d.CandidateTruncated {
			t.Fatalf("mid-name match in a large KB: %v %+v", err, d)
		}
		t.Logf("in-store substring filter over %d entities: %v", docs*(perDoc+1)+1, time.Since(start))
		// A common word matches more names than the cap: results still come
		// back, the cut is reported, and the exact match is kept and first.
		start = time.Now()
		g, d, err = r.Search(ctx, whole, []string{"总管-0", "阀"})
		if err != nil {
			t.Fatalf("truncated search failed: %v", err)
		}
		if !d.CandidateTruncated || d.IndexOnly || len(g.Relation) == 0 || g.Node[0].Name != "总管-0" {
			t.Fatalf("truncation: %+v, %d relations, first %q", d, len(g.Relation), g.Node[0].Name)
		}
		t.Logf("truncated search (%d candidates kept): %v %+v", d.CandidateSeeds, time.Since(start), d)
		if err := r.DelGraph(ctx, all); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("delete_checks_edge_ownership", func(t *testing.T) {
		a := types.NameSpace{KnowledgeBase: "kb-own", Knowledge: "doc-a"}
		b := types.NameSpace{KnowledgeBase: "kb-own", Knowledge: "doc-b"}
		for ns, rel := range map[types.NameSpace]*types.GraphRelation{
			a: {Node1: "甲", Node2: "乙", Type: "连接"}, b: {Node1: "丙", Node2: "丁", Type: "连接"},
		} {
			if err := r.AddGraph(ctx, ns, []*types.GraphData{{Relation: []*types.GraphRelation{rel}}}); err != nil {
				t.Fatal(err)
			}
			waitVisible(t, r, ns)
		}
		relA, _ := identity(cfg, "relation", a, "甲", "乙", "连接")
		entA, _ := identity(cfg, "entity", a, "甲")
		orphanRel, _ := identity(cfg, "relation", b, "已删除甲", "已删除乙", "连接")
		orphanEnt, _ := identity(cfg, "entity", b, "已删除甲")
		label := func(src, dst, doc string) {
			rawQuery(t, cfg, `INSERT EDGE primind_from(kb,doc) VALUES "`+src+`"->"`+dst+`":("kb-own","`+doc+`")`)
		}
		label(orphanRel, orphanEnt, "doc-b") // B's orphan: neither endpoint exists
		label(relA, entA, "doc-b")           // A's edge, now carrying B's label
		requireErr(t, r.DelGraph(ctx, []types.NameSpace{b}), ErrInvalid)
		for ns, name := range map[types.NameSpace]string{a: "甲", b: "丙"} {
			got, err := r.SearchNode(ctx, ns, []string{name})
			if err != nil || len(got.Relation) != 1 {
				t.Fatalf("%v damaged by the refused delete: %v", ns, err)
			}
		}
		label(relA, entA, "doc-a") // put A's label back
		if err := r.DelGraph(ctx, []types.NameSpace{b}); err != nil {
			t.Fatalf("delete with only an own orphan edge: %v", err)
		}
		rs := rawQuery(t, cfg, `FETCH PROP ON primind_from "`+orphanRel+`"->"`+orphanEnt+`" YIELD src(edge) AS s`)
		if rs.GetRowSize() != 0 {
			t.Fatal("B's orphan edge survived the delete")
		}
		snap, err := r.Snapshot(ctx, a)
		if err != nil || len(snap.Relation) != 1 {
			t.Fatalf("A after deleting B: %v", err)
		}
		if err := r.DelGraph(ctx, []types.NameSpace{a}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("name_key_whitespace", func(t *testing.T) { realNameKeyWhitespace(ctx, t, cfg, r) })
	t.Run("long_name_prefix", func(t *testing.T) { realLongNamePrefix(ctx, t, cfg, r) })
	t.Run("prefix_range", func(t *testing.T) { realPrefixRange(ctx, t, cfg, r) })

	t.Run("delete_all", func(t *testing.T) {
		var all []types.NameSpace
		for ns := range byNS {
			all = append(all, ns)
		}
		if err := r.DelGraph(ctx, all); err != nil {
			t.Fatal(err)
		}
		for _, ns := range all {
			deadline := time.Now().Add(30 * time.Second)
			for {
				snap, err := r.Snapshot(ctx, ns)
				if err == nil && len(snap.Node) == 0 && len(snap.Relation) == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%v not empty after delete: %v", ns, err)
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
	})
	if os.Getenv("NEBULA_IT_V2_KEEP") == "" {
		rawQuery(t, cfg, "DROP SPACE IF EXISTS "+space)
		return
	}
	// Keep the Space browsable: write the baseline back after delete_all.
	for ns, g := range byNS {
		if err := r.AddGraph(ctx, ns, []*types.GraphData{g}); err != nil {
			t.Fatalf("re-add %v: %v", ns, err)
		}
	}
}

func waitVisible(t *testing.T, r *Repository, ns types.NameSpace) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		snap, err := r.Snapshot(context.Background(), ns)
		if err == nil && len(snap.Node) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%v never became visible: %v", ns, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func rawSession(t *testing.T, cfg Config) (*ng.ConnectionPool, *ng.Session) {
	t.Helper()
	host, port, _ := strings.Cut(cfg.Addresses[0], ":")
	p := 0
	for _, c := range port {
		p = p*10 + int(c-'0')
	}
	pool, err := ng.NewConnectionPool([]ng.HostAddress{{Host: host, Port: p}}, ng.GetDefaultConf(), ng.DefaultLogger{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := pool.GetSession(cfg.Username, cfg.Password)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	return pool, s
}

func rawQuery(t *testing.T, cfg Config, stmt string) *ng.ResultSet {
	t.Helper()
	pool, s := rawSession(t, cfg)
	defer pool.Close()
	defer s.Release()
	var rs *ng.ResultSet
	var err error
	for i := 0; i < 30; i++ {
		if rs, err = s.Execute("USE " + cfg.Space + "; " + stmt); err == nil && rs.IsSucceed() {
			return rs
		}
		if strings.HasPrefix(stmt, "DROP SPACE") {
			rs, err = s.Execute(stmt)
			if err == nil && rs.IsSucceed() {
				return rs
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: %v %v", stmt, err, rs.GetErrorMsg())
	return nil
}

func columnValues(t *testing.T, rs *ng.ResultSet, col string) []string {
	t.Helper()
	vals, err := rs.GetValuesByColName(col)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i], _ = v.AsString()
	}
	return out
}

func recreateSpace(t *testing.T, cfg Config) {
	t.Helper()
	pool, s := rawSession(t, cfg)
	defer pool.Close()
	defer s.Release()
	for _, stmt := range []string{
		"DROP SPACE IF EXISTS " + cfg.Space,
		fmt.Sprintf("CREATE SPACE %s(partition_num=1, replica_factor=1, vid_type=FIXED_STRING(%d))", cfg.Space,
			cfg.VIDBytes),
	} {
		rs, err := s.Execute(stmt)
		if err != nil || !rs.IsSucceed() {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for i := 0; i < 60; i++ {
		if rs, err := s.Execute("USE " + cfg.Space); err == nil && rs.IsSucceed() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatal("space never became usable")
}
