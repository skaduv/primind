package nebula

import (
	"context"
	"fmt"
	"github.com/primind/primind/internal/types"
	"sort"
	"strings"
)

// Manifest is prepared before writes. It contains no credentials; names/IDs may
// still be business data and must be stored only in the authorized run folder.
type Manifest struct {
	RunID, Space string
	Namespaces   []types.NameSpace
	Vertices     []ManifestVertex
	Edges        []ManifestEdge
}
type ManifestVertex struct{ Tag, ID string }
type ManifestEdge struct{ Type, Source, Target string }
type GraphCall struct {
	Namespace types.NameSpace
	Graphs    []*types.GraphData
}

func BuildManifest(cfg Config, run string, calls []GraphCall) (Manifest, error) {
	if !schemaName.MatchString(run) {
		return Manifest{}, ErrInvalid
	}
	m := Manifest{RunID: run, Space: cfg.Space}
	vs := map[string]ManifestVertex{}
	es := map[string]ManifestEdge{}
	nsSet := map[types.NameSpace]bool{}
	for _, call := range calls {
		ns := call.Namespace
		if err := validateNamespace(ns, true); err != nil {
			return m, err
		}
		if !strings.HasPrefix(ns.KnowledgeBase, run+"-") || !strings.HasPrefix(ns.Knowledge, run+"-") {
			return m, fmt.Errorf("%w: run prefix", ErrInvalid)
		}
		if !nsSet[ns] {
			m.Namespaces = append(m.Namespaces, ns)
			nsSet[ns] = true
		}
		node := func(name string) (string, error) {
			if err := nonempty(name); err != nil {
				return "", err
			}
			id, err := identity(cfg, "entity", ns, name)
			if err == nil {
				vs[id] = ManifestVertex{entityTag, id}
			}
			return id, err
		}
		edge := func(typ, src, dst string) { es[typ+"|"+src+"|"+dst] = ManifestEdge{typ, src, dst} }
		for _, g := range call.Graphs {
			if g == nil {
				return m, ErrInvalid
			}
			for _, n := range g.Node {
				if n == nil {
					return m, ErrInvalid
				}
				nid, err := node(n.Name)
				if err != nil {
					return m, err
				}
				for _, chunk := range n.Chunks {
					if !strings.HasPrefix(chunk, run+"-") {
						return m, ErrInvalid
					}
					id, err := identity(cfg, "source", ns, n.Name, chunk)
					if err != nil {
						return m, err
					}
					vs[id] = ManifestVertex{sourceTag, id}
					edge("primind_owner", id, nid)
				}
			}
			for _, r := range g.Relation {
				if r == nil || r.Type == "" {
					return m, ErrInvalid
				}
				src, err := node(r.Node1)
				if err != nil {
					return m, err
				}
				dst, err := node(r.Node2)
				if err != nil {
					return m, err
				}
				id, err := identity(cfg, "relation", ns, r.Node1, r.Node2, r.Type)
				if err != nil {
					return m, err
				}
				vs[id] = ManifestVertex{relationTag, id}
				edge("primind_from", id, src)
				edge("primind_to", id, dst)
			}
		}
		if len(vs)+len(es) > cfg.SnapshotRecords*4 {
			return m, ErrBudget
		}
	}
	for _, v := range vs {
		m.Vertices = append(m.Vertices, v)
	}
	for _, e := range es {
		m.Edges = append(m.Edges, e)
	}
	sort.Slice(m.Vertices, func(i, j int) bool { return m.Vertices[i].ID < m.Vertices[j].ID })
	sort.Slice(m.Edges, func(i, j int) bool {
		a, b := m.Edges[i], m.Edges[j]
		return a.Type+a.Source+a.Target < b.Type+b.Source+b.Target
	})
	return m, nil
}

type Residue struct{ Vertices, Edges []string }

// ProbeManifest checks explicit identities, including edges whose vertices have
// disappeared. Namespace LOOKUP alone cannot prove that orphan edges are gone.
func (r *Repository) ProbeManifest(ctx context.Context, m Manifest) (Residue, error) {
	out := Residue{}
	if err := r.check(ctx); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	if m.Space != r.c.cfg.Space || !schemaName.MatchString(m.RunID) {
		return out, ErrInvalid
	}
	groups := map[string][]record{}
	for _, v := range m.Vertices {
		if _, ok := tagFields[v.Tag]; !ok || !safeVID(v.ID) {
			return out, ErrInvalid
		}
		groups[v.Tag] = append(groups[v.Tag], record{v.ID, literal(v.ID)})
	}
	for _, e := range m.Edges {
		allowed := false
		for _, name := range edgeNames {
			allowed = allowed || e.Type == name
		}
		if !allowed || !safeVID(e.Source) || !safeVID(e.Target) {
			return out, ErrInvalid
		}
		groups[e.Type] = append(groups[e.Type], record{e.Source + "->" + e.Target, literal(e.Source) + "->" + literal(e.Target)})
	}
	if len(m.Vertices)+len(m.Edges) > r.c.cfg.SnapshotRecords*4 {
		return out, ErrBudget
	}
	for _, kind := range append([]string{entityTag, relationTag, sourceTag}, edgeNames...) {
		_, vertex := tagFields[kind]
		suffix := " YIELD src(edge) AS src,dst(edge) AS dst;"
		if vertex {
			suffix = " YIELD id(vertex) AS vid;"
		}
		plan, err := splitBatches(r.c.cfg, "manifest_probe", "FETCH PROP ON "+kind+" ", suffix, groups[kind])
		if err != nil {
			return out, err
		}
		for _, b := range plan {
			tb, err := r.read(ctx, b.Kind, b.Statement)
			if err != nil {
				return out, err
			}
			want := []string{"src", "dst"}
			if vertex {
				want = []string{"vid"}
			}
			if err := columns(tb, want); err != nil {
				return out, err
			}
			allowed := map[string]bool{}
			for _, id := range b.IDs {
				allowed[id] = true
			}
			for _, row := range tb.Rows {
				if len(row) != len(want) {
					return out, ErrInvalid
				}
				id := strings.Join(row, "->")
				if !allowed[id] {
					return out, ErrInvalid
				}
				if vertex {
					out.Vertices = append(out.Vertices, id)
				} else {
					out.Edges = append(out.Edges, kind+":"+id)
				}
			}
		}
	}
	return out, nil
}
func safeVID(id string) bool {
	if !strings.HasPrefix(id, "ng1_") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
