package nebula

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

var _ interfaces.RetrieveGraphRepository = (*Repository)(nil)

const (
	entityTag   = "primind_entity"
	relationTag = "primind_relation"
	sourceTag   = "primind_source"
)

var tagFields = map[string][]string{
	entityTag:   {"kb", "doc", "name", "attrs"},
	relationTag: {"kb", "doc", "node1", "node2", "kind"},
	sourceTag:   {"kb", "doc", "name", "chunk"},
}
var edgeNames = []string{"primind_from", "primind_to", "primind_owner"}

func appendBatch(cfg Config, plan *[]batch, kind, prefix, suffix string, rs []record) error {
	bs, err := splitBatches(cfg, kind, prefix, suffix, rs)
	if err != nil {
		return err
	}
	*plan = append(*plan, bs...)
	return planBytes(cfg, *plan)
}

func vertexPrefix(tag string) string {
	return "INSERT VERTEX IF NOT EXISTS " + tag + "(" + strings.Join(tagFields[tag], ",") + ") VALUES "
}

func edgeRecord(ns types.NameSpace, id, from, to string) record {
	return record{id, literal(from) + "->" + literal(to) + ":" + values(ns.KnowledgeBase, ns.Knowledge)}
}

func (r *Repository) addPlan(ns types.NameSpace, graphs []*types.GraphData) ([]batch, error) {
	cfg := r.c.cfg
	// Bound planning before constructing encoded statements or spawning work.
	inputBytes, records := 0, 0
	for _, g := range graphs {
		if g == nil {
			return nil, ErrInvalid
		}
		for _, n := range g.Node {
			if n == nil {
				return nil, ErrInvalid
			}
			records++
			inputBytes += len(n.Name)
			for _, a := range n.Attributes {
				inputBytes += len(a)
			}
			for _, c := range n.Chunks {
				inputBytes += len(c)
				records++
			}
			if inputBytes > cfg.OperationBytes || records > cfg.SnapshotRecords {
				return nil, ErrBudget
			}
		}
		for _, e := range g.Relation {
			if e == nil {
				return nil, ErrInvalid
			}
			records++
			inputBytes += len(e.Node1) + len(e.Node2) + len(e.Type)
		}
		if inputBytes > cfg.OperationBytes || records > cfg.SnapshotRecords {
			return nil, ErrBudget
		}
	}
	var plan []batch
	for _, g := range graphs {
		if g == nil {
			return nil, fmt.Errorf("%w: nil graph", ErrInvalid)
		}
		var nodes, sources, rels, from, to, owners []record
		seen := map[string]bool{}
		seenSrc := map[string]bool{}
		seenRel := map[string]bool{}
		addNode := func(n *types.GraphNode) error {
			if n == nil {
				return fmt.Errorf("%w: nil node", ErrInvalid)
			}
			if err := nonempty(n.Name); err != nil {
				return err
			}
			for _, a := range n.Attributes {
				if !utf8.ValidString(a) {
					return ErrInvalid
				}
			}
			id, err := identity(cfg, "entity", ns, n.Name)
			if err != nil {
				return err
			}
			if !seen[id] {
				attrs, _ := json.Marshal(n.Attributes)
				nodes = append(nodes, record{id, literal(id) + ":" + values(ns.KnowledgeBase, ns.Knowledge, n.Name,
					string(attrs))})
				seen[id] = true
			}
			for _, chunk := range n.Chunks {
				if err := nonempty(chunk); err != nil {
					return err
				}
				sid, err := identity(cfg, "source", ns, n.Name, chunk)
				if err != nil {
					return err
				}
				if !seenSrc[sid] {
					sources = append(sources, record{sid, literal(sid) + ":" + values(ns.KnowledgeBase, ns.Knowledge,
						n.Name, chunk)})
					owners = append(owners, edgeRecord(ns, sid, sid, id))
					seenSrc[sid] = true
				}
			}
			return nil
		}
		for _, n := range g.Node {
			if err := addNode(n); err != nil {
				return nil, err
			}
		}
		for _, e := range g.Relation {
			if e == nil {
				return nil, ErrInvalid
			}
			if err := nonempty(e.Type); err != nil {
				return nil, err
			}
			if err := addNode(&types.GraphNode{Name: e.Node1}); err != nil {
				return nil, err
			}
			if err := addNode(&types.GraphNode{Name: e.Node2}); err != nil {
				return nil, err
			}
			id, err := identity(cfg, "relation", ns, e.Node1, e.Node2, e.Type)
			if err != nil {
				return nil, err
			}
			if seenRel[id] {
				continue
			}
			seenRel[id] = true
			src, _ := identity(cfg, "entity", ns, e.Node1)
			dst, _ := identity(cfg, "entity", ns, e.Node2)
			rels = append(rels, record{id, literal(id) + ":" + values(ns.KnowledgeBase, ns.Knowledge, e.Node1, e.Node2,
				e.Type)})
			from = append(from, edgeRecord(ns, id, id, src))
			to = append(to, edgeRecord(ns, id, id, dst))
		}
		for _, part := range []struct {
			tag     string
			records []record
		}{{entityTag, nodes}, {sourceTag, sources}, {relationTag, rels}} {
			if err := appendBatch(cfg, &plan, part.tag, vertexPrefix(part.tag), ";", part.records); err != nil {
				return nil, err
			}
		}
		for _, part := range []struct {
			edge    string
			records []record
		}{{"primind_owner", owners}, {"primind_from", from}, {"primind_to", to}} {
			if err := appendBatch(cfg, &plan, part.edge, "INSERT EDGE IF NOT EXISTS "+part.edge+"(kb,doc) VALUES ", ";",
				part.records); err != nil {
				return nil, err
			}
		}
	}
	return plan, nil
}

// AddGraph writes entities, relations and sources in bounded, dependency-ordered batches.
func (r *Repository) AddGraph(ctx context.Context, ns types.NameSpace, graphs []*types.GraphData) error {
	if err := r.check(ctx); err != nil {
		return err
	}
	if err := validateNamespace(ns, true); err != nil {
		return err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	defer func() {
		r.c.mu.Lock()
		r.c.metrics.AddCalls++
		r.c.metrics.AddWallNS += int64(time.Since(start))
		r.c.mu.Unlock()
	}()
	if len(graphs) == 0 {
		return nil
	}
	releasePlan, err := r.c.reservePlan(ctx)
	if err != nil {
		return err
	}
	defer releasePlan()
	plan, err := r.addPlan(ns, graphs)
	if err != nil {
		return err
	}
	return r.runPlan(ctx, "AddGraph", plan)
}

type storedRecord struct {
	ID, Tag string
	Fields  []string
}

func lookup(cfg Config, tag string, ns types.NameSpace) string {
	where := tag + ".kb == " + literal(encoded(ns.KnowledgeBase))
	if ns.Knowledge != "" {
		where += " AND " + tag + ".doc == " + literal(encoded(ns.Knowledge))
	}
	cols := []string{"id(vertex) AS vid"}
	for _, f := range tagFields[tag] {
		cols = append(cols, tag+"."+f+" AS "+f)
	}
	return "LOOKUP ON " + tag + " WHERE " + where + " YIELD " + strings.Join(cols,
		",") + " | LIMIT " + fmt.Sprint(cfg.SnapshotRecords+1) + ";"
}

func (r *Repository) read(ctx context.Context, kind, stmt string) (wireTable, error) {
	if err := r.check(ctx); err != nil {
		return wireTable{}, err
	}
	if len(stmt) > r.c.cfg.StatementBytes {
		return wireTable{}, ErrBudget
	}
	o := r.c.execute(ctx, batch{Kind: kind, Statement: stmt}, false)
	return o.Table, o.Err
}

func (r *Repository) rawSnapshot(ctx context.Context, ns types.NameSpace) ([]storedRecord, error) {
	var out []storedRecord
	bytes := 0
	for _, tag := range []string{entityTag, relationTag, sourceTag} {
		table, err := r.read(ctx, "snapshot", lookup(r.c.cfg, tag, ns))
		if err != nil {
			return nil, err
		}
		want := append([]string{"vid"}, tagFields[tag]...)
		if err := columns(table, want); err != nil {
			return nil, err
		}
		for _, row := range table.Rows {
			if len(out) >= r.c.cfg.SnapshotRecords {
				return nil, ErrBudget
			}
			if len(row) != len(want) {
				return nil, ErrInvalid
			}
			item := storedRecord{ID: row[0], Tag: tag}
			for _, v := range row[1:] {
				value, err := decoded(v)
				if err != nil {
					return nil, err
				}
				item.Fields = append(item.Fields, value)
				bytes += len(value)
			}
			if bytes > r.c.cfg.ReadBytes {
				return nil, ErrBudget
			}
			if item.Fields[0] != ns.KnowledgeBase || (ns.Knowledge != "" && item.Fields[1] != ns.Knowledge) ||
				item.Fields[1] == "" {
				return nil, fmt.Errorf("%w: namespace leakage", ErrInvalid)
			}
			actualNS := types.NameSpace{KnowledgeBase: item.Fields[0], Knowledge: item.Fields[1]}
			role := "entity"
			parts := []string{item.Fields[2]}
			switch tag {
			case relationTag:
				role = "relation"
				parts = item.Fields[2:]
			case sourceTag:
				role = "source"
				parts = item.Fields[2:]
			}
			id, err := identity(r.c.cfg, role, actualNS, parts...)
			if err != nil || id != item.ID {
				return nil, fmt.Errorf("%w: stored identity", ErrInvalid)
			}
			out = append(out, item)
		}
	}
	return out, nil
}

func columns(t wireTable, want []string) error {
	if len(t.Columns) != len(want) {
		return fmt.Errorf("%w: result columns", ErrInvalid)
	}
	for i, v := range want {
		if t.Columns[i] != v {
			return fmt.Errorf("%w: result column", ErrInvalid)
		}
	}
	return nil
}

// Snapshot is bounded diagnostic reconciliation, without SearchNode's 200/2000
// caps. It refuses incomplete/dangling records instead of claiming completeness.
func (r *Repository) Snapshot(ctx context.Context, ns types.NameSpace) (*types.GraphData, error) {
	if err := r.check(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	if err := validateNamespace(ns, false); err != nil {
		return nil, err
	}
	rows, err := r.rawSnapshot(ctx, ns)
	if err != nil {
		return nil, err
	}
	graph := &types.GraphData{}
	nodes := map[string]*types.GraphNode{}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.ID] {
			return nil, fmt.Errorf("%w: duplicate identity", ErrInvalid)
		}
		seen[row.ID] = true
		if row.Tag != entityTag {
			continue
		}
		node := &types.GraphNode{ID: row.ID, KnowledgeID: row.Fields[1], Name: row.Fields[2]}
		if err := nonempty(node.Name); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(row.Fields[3]), &node.Attributes); err != nil {
			return nil, fmt.Errorf("%w: attributes JSON", ErrInvalid)
		}
		nodes[row.ID] = node
		graph.Node = append(graph.Node, node)
	}
	edges := map[string][]record{}
	for _, row := range rows {
		n := types.NameSpace{KnowledgeBase: row.Fields[0], Knowledge: row.Fields[1]}
		switch row.Tag {
		case relationTag:
			for _, v := range row.Fields[2:] {
				if err := nonempty(v); err != nil {
					return nil, err
				}
			}
			src, _ := identity(r.c.cfg, "entity", n, row.Fields[2])
			dst, _ := identity(r.c.cfg, "entity", n, row.Fields[3])
			if nodes[src] == nil || nodes[dst] == nil {
				return nil, fmt.Errorf("%w: missing endpoint", ErrInvalid)
			}
			graph.Relation = append(graph.Relation, &types.GraphRelation{
				ID: row.ID, SourceID: src, TargetID: dst,
				Node1: row.Fields[2], Node2: row.Fields[3], Type: row.Fields[4],
			})
			edges["primind_from"] = append(edges["primind_from"], record{row.ID, literal(row.ID) + "->" + literal(src)})
			edges["primind_to"] = append(edges["primind_to"], record{row.ID, literal(row.ID) + "->" + literal(dst)})
		case sourceTag:
			id, _ := identity(r.c.cfg, "entity", n, row.Fields[2])
			node := nodes[id]
			if node == nil || row.Fields[3] == "" {
				return nil, fmt.Errorf("%w: missing source owner", ErrInvalid)
			}
			node.Chunks = append(node.Chunks, row.Fields[3])
			edges["primind_owner"] = append(edges["primind_owner"], record{
				row.ID,
				literal(row.ID) + "->" + literal(id),
			})
		}
	}
	actualEdges, err := r.rawEdges(ctx, ns)
	if err != nil {
		return nil, err
	}
	if len(actualEdges)+len(rows) > r.c.cfg.SnapshotRecords {
		return nil, ErrBudget
	}
	expectedEdges := map[string]bool{}
	for kind, records := range edges {
		for _, rec := range records {
			expectedEdges[kind+"|"+rec.Value] = true
		}
	}
	if len(actualEdges) != len(expectedEdges) {
		return nil, fmt.Errorf("%w: missing or orphan internal edges", ErrInvalid)
	}
	for _, edge := range actualEdges {
		if !expectedEdges[edge.Kind+"|"+literal(edge.Source)+"->"+literal(edge.Target)] {
			return nil, fmt.Errorf("%w: internal edge mismatch", ErrInvalid)
		}
	}

	for _, n := range graph.Node {
		sort.Strings(n.Chunks)
	}
	sort.Slice(graph.Node, func(i, j int) bool { return graph.Node[i].ID < graph.Node[j].ID })
	sort.Slice(graph.Relation, func(i, j int) bool { return graph.Relation[i].ID < graph.Relation[j].ID })
	return graph, nil
}

// SearchDiagnostics reports truncation observed during a search.
type SearchDiagnostics struct {
	SnapshotNodes, SnapshotRelations                 int
	CandidateSeeds, SelectedSeeds, ReturnedRelations int
	SeedTruncated, RelationTruncated                 bool
}

// SearchNode implements interfaces.RetrieveGraphRepository.
func (r *Repository) SearchNode(ctx context.Context, ns types.NameSpace, names []string) (*types.GraphData, error) {
	graph, _, err := r.Search(ctx, ns, names)
	return graph, err
}

// Search is SearchNode with truncation diagnostics.
func (r *Repository) Search(ctx context.Context, ns types.NameSpace, names []string) (*types.GraphData,
	SearchDiagnostics, error,
) {
	d := SearchDiagnostics{}
	if err := r.check(ctx); err != nil {
		return nil, d, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	if err := validateNamespace(ns, false); err != nil {
		return nil, d, err
	}
	if len(names) == 0 {
		return &types.GraphData{}, d, nil
	}
	if len(names) > 200 {
		return nil, d, ErrBudget
	}
	queryBytes := 0
	for _, name := range names {
		if err := nonempty(name); err != nil {
			return nil, d, err
		}
		queryBytes += len(name)
	}
	if queryBytes > r.c.cfg.StatementBytes {
		return nil, d, ErrBudget
	}
	all, err := r.Snapshot(ctx, ns)
	if err != nil {
		return nil, d, err
	}
	d.SnapshotNodes = len(all.Node)
	d.SnapshotRelations = len(all.Relation)
	connected := map[string]bool{}
	for _, e := range all.Relation {
		connected[e.SourceID] = true
		connected[e.TargetID] = true
	}
	var seeds []*types.GraphNode
	exact := func(s string) bool {
		for _, q := range names {
			if s == q {
				return true
			}
		}
		return false
	}
	for _, n := range all.Node {
		if !connected[n.ID] {
			continue
		}
		for _, q := range names {
			if strings.Contains(n.Name, q) {
				seeds = append(seeds, n)
				break
			}
		}
	}
	sort.Slice(seeds, func(i, j int) bool {
		a, b := seeds[i], seeds[j]
		if exact(a.Name) != exact(b.Name) {
			return exact(a.Name)
		}
		if utf8.RuneCountInString(a.Name) != utf8.RuneCountInString(b.Name) {
			return utf8.RuneCountInString(a.Name) < utf8.RuneCountInString(b.Name)
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.KnowledgeID != b.KnowledgeID {
			return a.KnowledgeID < b.KnowledgeID
		}
		return a.ID < b.ID
	})
	d.CandidateSeeds = len(seeds)
	if len(seeds) > 200 {
		seeds = seeds[:200]
		d.SeedTruncated = true
	}
	d.SelectedSeeds = len(seeds)
	out := &types.GraphData{}
	nodeMap := map[string]*types.GraphNode{}
	for _, n := range all.Node {
		nodeMap[n.ID] = n
	}
	emittedNodes := map[string]bool{}
	emittedEdges := map[string]bool{}
	emit := func(id string) {
		if !emittedNodes[id] {
			out.Node = append(out.Node, nodeMap[id])
			emittedNodes[id] = true
		}
	}
	for _, s := range seeds {
		for _, e := range all.Relation {
			if emittedEdges[e.ID] || (e.SourceID != s.ID && e.TargetID != s.ID) {
				continue
			}
			if len(out.Relation) == 2000 {
				d.RelationTruncated = true
				continue
			}
			emittedEdges[e.ID] = true
			emit(s.ID)
			emit(e.SourceID)
			emit(e.TargetID)
			out.Relation = append(out.Relation, e)
		}
	}
	d.ReturnedRelations = len(out.Relation)
	if d.SeedTruncated || d.RelationTruncated {
		r.c.mu.Lock()
		r.c.metrics.SearchTruncations++
		r.c.mu.Unlock()
	}
	return out, d, nil
}

// DelGraph deletes every record of the given namespaces; repeating it is harmless.
func (r *Repository) DelGraph(ctx context.Context, namespaces []types.NameSpace) error {
	if err := r.check(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, r.c.cfg.OperationTimeout)
	defer cancel()
	for _, ns := range namespaces {
		if err := validateNamespace(ns, true); err != nil {
			return err
		}
	}
	if len(namespaces) == 0 {
		return nil
	}
	releasePlan, err := r.c.reservePlan(ctx)
	if err != nil {
		return err
	}
	defer releasePlan()
	var plan []batch
	for _, ns := range namespaces {
		rows, err := r.rawSnapshot(ctx, ns)
		if err != nil {
			return err
		}
		edges, err := r.rawEdges(ctx, ns)
		if err != nil {
			return err
		}
		if len(rows)+len(edges) > r.c.cfg.SnapshotRecords {
			return ErrBudget
		}
		for _, kind := range edgeNames {
			var records []record
			for _, edge := range edges {
				if edge.Kind == kind {
					records = append(records, record{
						edge.Source + "->" + edge.Target,
						literal(edge.Source) + "->" + literal(edge.Target),
					})
				}
			}
			if err := appendBatch(r.c.cfg, &plan, "delete_edge", "DELETE EDGE "+kind+" ", ";", records); err != nil {
				return err
			}
		}
		var records []record
		for _, row := range rows {
			records = append(records, record{row.ID, literal(row.ID)})
		}
		if err := appendBatch(r.c.cfg, &plan, "delete", "DELETE VERTEX ", " WITH EDGE;", records); err != nil {
			return err
		}
	}
	return r.runPlan(ctx, "DelGraph", plan)
}
