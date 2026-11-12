package nebula

import (
	"context"
	"fmt"

	"github.com/primind/primind/internal/types"
)

var edgeColumns = []string{"src", "dst", "rank", "kb", "doc"}

type storedEdge struct{ Kind, Source, Target string }

func edgeLookup(cfg Config, kind string, ns types.NameSpace) (string, map[string]string) {
	where, params := scopeWhere(kind, ns)
	return "LOOKUP ON " + kind + " WHERE " + where + " YIELD src(edge) AS src,dst(edge) AS dst,rank(edge) AS rank," +
		kind + ".kb AS kb," + kind + ".doc AS doc | LIMIT " + fmt.Sprint(cfg.SnapshotRecords+1) + ";", params
}

func (r *Repository) rawEdges(ctx context.Context, ns types.NameSpace) ([]storedEdge, error) {
	var out []storedEdge
	seen := map[string]bool{}
	bytes := 0
	for _, kind := range edgeNames {
		stmt, params := edgeLookup(r.c.cfg, kind, ns)
		tb, err := r.read(ctx, "edge_snapshot", stmt, params)
		if err != nil {
			return nil, err
		}
		if err := columns(tb, edgeColumns); err != nil {
			return nil, err
		}
		for _, row := range tb.Rows {
			if len(row) != 5 || row[2] != "0" {
				return nil, ErrInvalid
			}
			kb, doc := row[3], row[4]
			if kb != ns.KnowledgeBase || (ns.Knowledge != "" && doc != ns.Knowledge) {
				return nil, ErrInvalid
			}
			// VIDs are opaque digests: only their shape can be checked here.
			// Who an edge really belongs to is established by the callers
			// (Snapshot against the expected edge set, DelGraph via foreignEndpoints).
			if !safeVID(row[0]) || !safeVID(row[1]) {
				return nil, fmt.Errorf("%w: edge endpoint VID", ErrInvalid)
			}
			key := kind + "|" + row[0] + "|" + row[1]
			if seen[key] {
				return nil, ErrInvalid
			}
			seen[key] = true
			bytes += len(key) + len(kb) + len(doc)
			if len(out) >= r.c.cfg.SnapshotRecords || bytes > r.c.cfg.ReadBytes {
				return nil, ErrBudget
			}
			out = append(out, storedEdge{kind, row[0], row[1]})
		}
	}
	return out, nil
}

// foreignEndpoints reports whether any edge labelled with ns ends on a vertex
// that exists but belongs to another namespace. Edge labels (kb, doc) are the
// only thing a namespace LOOKUP sees, and VIDs cannot be decoded, so before
// deleting an edge its endpoints are checked independently:
//
//   - both endpoints among the namespace's own vertices: the edge is ours;
//   - an endpoint that no longer exists: an orphan edge, safe to delete;
//   - an endpoint that exists under another kb/doc: someone else's edge that
//     was mislabelled; deleting it would break their graph, so refuse.
//
// Only endpoints outside owned are fetched, which is none in a healthy graph.
func (r *Repository) foreignEndpoints(ctx context.Context, ns types.NameSpace, owned map[string]bool,
	edges []storedEdge,
) error {
	var unknown []string
	seen := map[string]bool{}
	for _, e := range edges {
		for _, id := range []string{e.Source, e.Target} {
			if !owned[id] && !seen[id] {
				seen[id] = true
				unknown = append(unknown, id)
			}
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	for _, tag := range []string{entityTag, relationTag, sourceTag} {
		rows, err := r.readMany(ctx, "delete_owner", "FETCH PROP ON "+tag+" ",
			" YIELD id(vertex) AS vid,"+tag+".kb AS kb,"+tag+".doc AS doc;", unknown, []string{"vid", "kb", "doc"})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if !seen[row[0]] {
				return fmt.Errorf("%w: endpoint lookup row", ErrInvalid)
			}
			if row[1] != ns.KnowledgeBase || row[2] != ns.Knowledge {
				return fmt.Errorf("%w: edge labelled for this namespace ends on another namespace's vertex",
					ErrInvalid)
			}
		}
	}
	return nil
}
