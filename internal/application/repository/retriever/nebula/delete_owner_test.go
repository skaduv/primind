package nebula

import (
	"context"
	"strings"
	"testing"

	"github.com/primind/primind/internal/types"
)

// deleteWire is the golden baseline for ns plus one extra primind_from edge
// that carries ns's label. foreign lists vertices that exist outside ns:
// VID -> {tag, kb, doc}. It records every DELETE statement instead of running it.
func deleteWire(t *testing.T, cfg Config, ns types.NameSpace, extra [2]string, foreign map[string][3]string,
) (*testWire, *[]string) {
	t.Helper()
	w := goldenWire(t, cfg, loadGolden(t).Baseline, ns)
	read := w.fn
	var deletes []string
	w.fn = func(ctx context.Context, s string) wireOutcome {
		if strings.HasPrefix(s, "DELETE ") {
			deletes = append(deletes, s)
			return wireOutcome{Sent: true}
		}
		out := read(ctx, s)
		if strings.HasPrefix(s, "LOOKUP ON primind_from ") && extra[0] != "" {
			out.Table.Rows = append(out.Table.Rows, []string{extra[0], extra[1], "0", ns.KnowledgeBase, ns.Knowledge})
		}
		for id, v := range foreign {
			if strings.HasPrefix(s, "FETCH PROP ON "+v[0]+" ") && strings.Contains(s, literal(id)) {
				out.Table.Rows = append(out.Table.Rows, []string{id, v[1], v[2]})
			}
		}
		return out
	}
	return w, &deletes
}

func ownerFetches(w *testWire) int {
	n := 0
	for _, s := range w.stmts {
		if strings.HasPrefix(s, "FETCH PROP ON ") {
			n++
		}
	}
	return n
}

// A healthy namespace is deleted without any extra endpoint lookups.
func TestDeleteHealthyNamespaceNeedsNoOwnerLookup(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	w, deletes := deleteWire(t, cfg, ns, [2]string{}, nil)
	if err := testRepo(cfg, w).DelGraph(context.Background(), []types.NameSpace{ns}); err != nil {
		t.Fatal(err)
	}
	if ownerFetches(w) != 0 || len(*deletes) == 0 {
		t.Fatalf("owner lookups %d, deletes %d", ownerFetches(w), len(*deletes))
	}
}

// An edge labelled B whose endpoints no longer exist anywhere is B's orphan:
// deleting B removes it.
func TestDeleteRemovesOwnOrphanEdge(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	rel, _ := identity(cfg, "relation", ns, "已删除的实体甲", "已删除的实体乙", "连接")
	ent, _ := identity(cfg, "entity", ns, "已删除的实体甲")
	w, deletes := deleteWire(t, cfg, ns, [2]string{rel, ent}, nil)
	if err := testRepo(cfg, w).DelGraph(context.Background(), []types.NameSpace{ns}); err != nil {
		t.Fatal(err)
	}
	if ownerFetches(w) == 0 {
		t.Fatal("unknown endpoints were not looked up")
	}
	found := false
	for _, s := range *deletes {
		found = found || (strings.HasPrefix(s, "DELETE EDGE primind_from ") &&
			strings.Contains(s, literal(rel)+"->"+literal(ent)))
	}
	if !found {
		t.Fatal("orphan edge was not deleted")
	}
}

// An edge that belongs to document A but carries B's label must survive the
// deletion of B: its endpoints exist under A, so the whole delete is refused
// before anything is removed.
func TestDeleteRefusesEdgeOfAnotherDocumentLabelledAsOurs(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	for name, other := range map[string]types.NameSpace{
		"other document": {KnowledgeBase: ns.KnowledgeBase, Knowledge: "doc-a2"},
		"other tenant":   {KnowledgeBase: "kb-b", Knowledge: ns.Knowledge},
	} {
		t.Run(name, func(t *testing.T) {
			rel, _ := identity(cfg, "relation", other, "甲", "乙", "连接")
			ent, _ := identity(cfg, "entity", other, "甲")
			for _, foreign := range []map[string][3]string{
				{ // both endpoints still exist under the other namespace
					rel: {relationTag, other.KnowledgeBase, other.Knowledge},
					ent: {entityTag, other.KnowledgeBase, other.Knowledge},
				},
				{ent: {entityTag, other.KnowledgeBase, other.Knowledge}}, // only one of them does
			} {
				w, deletes := deleteWire(t, cfg, ns, [2]string{rel, ent}, foreign)
				err := testRepo(cfg, w).DelGraph(context.Background(), []types.NameSpace{ns})
				requireErr(t, err, ErrInvalid)
				if len(*deletes) != 0 {
					t.Fatalf("deleted despite a foreign edge: %v", *deletes)
				}
			}
		})
	}
}

// VIDs read back from the store are spliced into DELETE statements, so one
// that does not have the exact digest shape stops the delete.
func TestDeleteRejectsMalformedEndpointVID(t *testing.T) {
	cfg := testConfig()
	ns := testNS()
	ent, _ := identity(cfg, "entity", ns, "甲")
	for _, bad := range []string{`x" OR "1"=="1`, "ng1_" + ent[4:], ent + "A", ""} {
		w, deletes := deleteWire(t, cfg, ns, [2]string{"placeholder", ent}, nil)
		read := w.fn
		w.fn = func(ctx context.Context, s string) wireOutcome {
			out := read(ctx, s)
			for _, row := range out.Table.Rows {
				if len(row) == 5 && row[0] == "placeholder" {
					row[0] = bad
				}
			}
			return out
		}
		requireErr(t, testRepo(cfg, w).DelGraph(context.Background(), []types.NameSpace{ns}), ErrInvalid)
		if len(*deletes) != 0 {
			t.Fatalf("deleted with endpoint %q", bad)
		}
	}
}
