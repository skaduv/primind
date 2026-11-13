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
	// entityKeyIndex serves seed lookup by normalised name inside one KB.
	entityKeyIndex = "primind_entity_key"

	searchMaxSeeds     = 200
	searchMaxRelations = 2000
	// largeScopeTTL bounds how long a namespace stays on the index-only path
	// before search tries the substring filter again (deletes can shrink it).
	largeScopeTTL = 10 * time.Minute
)

var tagFields = map[string][]string{
	entityTag:   {"kb", "doc", "name", "name_key", "attrs"},
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

func vertexRecord(id string, vals ...string) record {
	return record{ID: id, Value: literal(id), Vals: vals}
}

func edgeRecord(ns types.NameSpace, id, from, to string) record {
	return record{ID: id, Value: literal(from) + "->" + literal(to), Vals: []string{ns.KnowledgeBase, ns.Knowledge}}
}

func (r *Repository) addPlan(ns types.NameSpace, graphs []*types.GraphData) ([]batch, error) {
	cfg := r.c.cfg
	// Bound planning before constructing statements or spawning work.
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
			inputBytes += 2 * len(n.Name) // name + name_key
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
	kb, doc := ns.KnowledgeBase, ns.Knowledge
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
				nodes = append(nodes, vertexRecord(id, kb, doc, n.Name, nameKey(n.Name), string(attrs)))
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
					sources = append(sources, vertexRecord(sid, kb, doc, n.Name, chunk))
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
			rels = append(rels, vertexRecord(id, kb, doc, e.Node1, e.Node2, e.Type))
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

func lookup(cfg Config, tag string, ns types.NameSpace) (string, map[string]string) {
	where, params := scopeWhere(tag, ns)
	cols := []string{"id(vertex) AS vid"}
	for _, f := range tagFields[tag] {
		cols = append(cols, tag+"."+f+" AS "+f)
	}
	return "LOOKUP ON " + tag + " WHERE " + where + " YIELD " + strings.Join(cols,
		",") + " | LIMIT " + fmt.Sprint(cfg.SnapshotRecords+1) + ";", params
}

func (r *Repository) read(ctx context.Context, kind, stmt string, params map[string]string) (wireTable, error) {
	if err := r.check(ctx); err != nil {
		return wireTable{}, err
	}
	b := batch{Kind: kind, Statement: stmt, Params: params}
	if b.size() > r.c.cfg.StatementBytes {
		return wireTable{}, ErrBudget
	}
	o := r.c.execute(ctx, b, false)
	return o.Table, o.Err
}

// readMany runs one read per batch of literal VIDs and checks the columns.
func (r *Repository) readMany(ctx context.Context, kind, prefix, suffix string, ids []string,
	want []string,
) ([][]string, error) {
	recs := make([]record, len(ids))
	for i, id := range ids {
		if !safeVID(id) {
			return nil, fmt.Errorf("%w: VID", ErrInvalid)
		}
		recs[i] = record{ID: id, Value: literal(id)}
	}
	plan, err := splitBatches(r.c.cfg, kind, prefix, suffix, recs)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, b := range plan {
		tb, err := r.read(ctx, b.Kind, b.Statement, b.Params)
		if err != nil {
			return nil, err
		}
		if err := columns(tb, want); err != nil {
			return nil, err
		}
		for _, row := range tb.Rows {
			if len(row) != len(want) {
				return nil, ErrInvalid
			}
		}
		rows = append(rows, tb.Rows...)
	}
	return rows, nil
}

// verifyRecord checks one stored vertex: UTF-8 plaintext, namespace, the
// entity search key, and that the VID recomputed from the properties matches.
func verifyRecord(cfg Config, tag string, ns types.NameSpace, id string, fields []string) error {
	for _, v := range fields {
		if !utf8.ValidString(v) {
			return fmt.Errorf("%w: property UTF-8", ErrInvalid)
		}
	}
	if fields[0] != ns.KnowledgeBase || (ns.Knowledge != "" && fields[1] != ns.Knowledge) || fields[1] == "" {
		return fmt.Errorf("%w: namespace leakage", ErrInvalid)
	}
	actual := types.NameSpace{KnowledgeBase: fields[0], Knowledge: fields[1]}
	var parts []string
	switch tag {
	case entityTag:
		if fields[3] != nameKey(fields[2]) {
			return fmt.Errorf("%w: stored name_key", ErrInvalid)
		}
		parts = fields[2:3]
	case relationTag, sourceTag:
		parts = fields[2:]
	}
	for _, p := range parts {
		if err := nonempty(p); err != nil {
			return err
		}
	}
	role := map[string]string{entityTag: "entity", relationTag: "relation", sourceTag: "source"}[tag]
	want, err := identity(cfg, role, actual, parts...)
	if err != nil || want != id {
		return fmt.Errorf("%w: stored identity", ErrInvalid)
	}
	return nil
}

func (r *Repository) rawSnapshot(ctx context.Context, ns types.NameSpace) ([]storedRecord, error) {
	var out []storedRecord
	bytes := 0
	for _, tag := range []string{entityTag, relationTag, sourceTag} {
		stmt, params := lookup(r.c.cfg, tag, ns)
		table, err := r.read(ctx, "snapshot", stmt, params)
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
			item := storedRecord{ID: row[0], Tag: tag, Fields: append([]string(nil), row[1:]...)}
			for _, v := range item.Fields {
				bytes += len(v)
			}
			if bytes > r.c.cfg.ReadBytes {
				return nil, ErrBudget
			}
			if err := verifyRecord(r.c.cfg, tag, ns, item.ID, item.Fields); err != nil {
				return nil, err
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

func entityNode(row storedRecord) (*types.GraphNode, error) {
	node := &types.GraphNode{ID: row.ID, KnowledgeID: row.Fields[1], Name: row.Fields[2]}
	if err := json.Unmarshal([]byte(row.Fields[4]), &node.Attributes); err != nil {
		return nil, fmt.Errorf("%w: attributes JSON", ErrInvalid)
	}
	return node, nil
}

// Snapshot is bounded diagnostic reconciliation of a whole namespace. It
// refuses incomplete/dangling records instead of claiming completeness. It is
// not on the search path.
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
		node, err := entityNode(row)
		if err != nil {
			return nil, err
		}
		nodes[row.ID] = node
		graph.Node = append(graph.Node, node)
	}
	edges := map[string][]record{}
	for _, row := range rows {
		n := types.NameSpace{KnowledgeBase: row.Fields[0], Knowledge: row.Fields[1]}
		switch row.Tag {
		case relationTag:
			src, _ := identity(r.c.cfg, "entity", n, row.Fields[2])
			dst, _ := identity(r.c.cfg, "entity", n, row.Fields[3])
			if nodes[src] == nil || nodes[dst] == nil {
				return nil, fmt.Errorf("%w: missing endpoint", ErrInvalid)
			}
			graph.Relation = append(graph.Relation, &types.GraphRelation{
				ID: row.ID, SourceID: src, TargetID: dst,
				Node1: row.Fields[2], Node2: row.Fields[3], Type: row.Fields[4],
			})
			edges["primind_from"] = append(edges["primind_from"],
				record{ID: row.ID, Value: literal(row.ID) + "->" + literal(src)})
			edges["primind_to"] = append(edges["primind_to"],
				record{ID: row.ID, Value: literal(row.ID) + "->" + literal(dst)})
		case sourceTag:
			id, _ := identity(r.c.cfg, "entity", n, row.Fields[2])
			node := nodes[id]
			if node == nil {
				return nil, fmt.Errorf("%w: missing source owner", ErrInvalid)
			}
			node.Chunks = append(node.Chunks, row.Fields[3])
			edges["primind_owner"] = append(edges["primind_owner"], record{
				ID:    row.ID,
				Value: literal(row.ID) + "->" + literal(id),
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

// SearchDiagnostics reports how a search was answered and any truncation.
type SearchDiagnostics struct {
	// CandidateSeeds counts name matches; ConnectedSeeds those with at least
	// one relation among the candidates examined.
	CandidateSeeds, ConnectedSeeds, SelectedSeeds, ReturnedRelations int
	// Each stage that can cut results has its own flag, in pipeline order:
	// CandidateTruncated when more names matched than SnapshotRecords,
	// SeedTruncated when more than searchMaxSeeds matches have relations,
	// RelationTruncated when they have more than searchMaxRelations.
	CandidateTruncated, SeedTruncated, RelationTruncated bool
	// IndexOnly is set when the in-store substring filter did not finish
	// within SearchScanTimeout: seeds then come from the name_key index
	// (exact or prefix match) only.
	IndexOnly bool
}

// SearchNode implements interfaces.RetrieveGraphRepository.
func (r *Repository) SearchNode(ctx context.Context, ns types.NameSpace, names []string) (*types.GraphData, error) {
	graph, _, err := r.Search(ctx, ns, names)
	return graph, err
}

type seedCandidate struct {
	id, name, doc string
	exact         bool
}

// Search finds entities whose normalised name contains a normalised query
// name, then returns every relation one hop from them, all evaluated inside
// NebulaGraph. Only the returned records are verified; full-namespace
// reconciliation is Snapshot's job.
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
	if len(names) > searchMaxSeeds {
		return nil, d, ErrBudget
	}
	queryBytes := 0
	var keys []string
	seenKey := map[string]bool{}
	for _, name := range names {
		if err := nonempty(name); err != nil {
			return nil, d, err
		}
		queryBytes += len(name)
		if k := nameKey(name); k != "" && !seenKey[k] {
			seenKey[k] = true
			keys = append(keys, k)
		}
	}
	if queryBytes > r.c.cfg.StatementBytes {
		return nil, d, ErrBudget
	}
	s := &searchRun{r: r, ns: ns, keys: keys, d: &d}
	graph, err := s.run(ctx)
	if err != nil {
		return nil, d, err
	}
	if d.CandidateTruncated || d.SeedTruncated || d.RelationTruncated {
		r.c.mu.Lock()
		r.c.metrics.SearchTruncations++
		r.c.mu.Unlock()
	}
	return graph, d, nil
}

type searchRun struct {
	r         *Repository
	ns        types.NameSpace
	keys      []string
	d         *SearchDiagnostics
	readBytes int
}

func (s *searchRun) account(rows [][]string) error {
	for _, row := range rows {
		for _, v := range row {
			s.readBytes += len(v)
		}
	}
	if s.readBytes > s.r.c.cfg.ReadBytes {
		return ErrBudget
	}
	return nil
}

func (s *searchRun) run(ctx context.Context) (*types.GraphData, error) {
	out := &types.GraphData{}
	if len(s.keys) == 0 {
		return out, nil
	}
	cands, err := s.candidates(ctx)
	if err != nil {
		return nil, err
	}
	s.d.CandidateSeeds = len(cands)
	seeds, relsOf, err := s.connectedSeeds(ctx, cands)
	if err != nil {
		return nil, err
	}
	// Relations in seed order; within a seed by relation identity. Each
	// relation is emitted once, from its earliest seed.
	var relIDs []string
	emitted := map[string]bool{}
	for _, seed := range seeds {
		for _, rel := range relsOf[seed.id] {
			if emitted[rel] {
				continue
			}
			if len(relIDs) == searchMaxRelations {
				s.d.RelationTruncated = true
				continue
			}
			emitted[rel] = true
			relIDs = append(relIDs, rel)
		}
	}
	if len(relIDs) == 0 {
		return out, nil
	}
	rels, nodes, err := s.relationsAndEntities(ctx, relIDs)
	if err != nil {
		return nil, err
	}
	endpointIDs := make([]string, 0, len(nodes))
	for id := range nodes {
		endpointIDs = append(endpointIDs, id)
	}
	sort.Strings(endpointIDs)
	if err := s.sources(ctx, endpointIDs, nodes); err != nil {
		return nil, err
	}
	// Node order: for each relation, its seed first, then both endpoints.
	seedOf := map[string]string{}
	for _, seed := range seeds {
		for _, rel := range relsOf[seed.id] {
			if _, ok := seedOf[rel]; !ok {
				seedOf[rel] = seed.id
			}
		}
	}
	emittedNode := map[string]bool{}
	emit := func(id string) {
		if !emittedNode[id] {
			emittedNode[id] = true
			out.Node = append(out.Node, nodes[id])
		}
	}
	for _, id := range relIDs {
		rel := rels[id]
		emit(seedOf[id])
		emit(rel.SourceID)
		emit(rel.TargetID)
		out.Relation = append(out.Relation, rel)
	}
	s.d.ReturnedRelations = len(out.Relation)
	return out, nil
}

func (s *searchRun) isLargeScope() bool {
	c := s.r.c
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.largeScopes[s.ns]
	return ok && time.Now().Before(until)
}

func (s *searchRun) markLargeScope() {
	c := s.r.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.largeScopes == nil {
		c.largeScopes = map[types.NameSpace]time.Time{}
	}
	if len(c.largeScopes) > 4096 { // bound memory; entries are only an optimisation
		clear(c.largeScopes)
	}
	c.largeScopes[s.ns] = time.Now().Add(largeScopeTTL)
}

// candidateColumns is what every seed query returns for one entity. The name
// and document come from properties: a VID is a digest and cannot be decoded.
var candidateColumns = []string{"vid", "doc", "name", "name_key"}

// prefixSeedStatement is the prefix fallback's seed statement for key i,
// whose value is parameter $q<i>. The index range only narrows the scan (see
// prefixRange); STARTS WITH on the full key in the pipe decides the match.
// It adds the bounds to params as $lo<i> and $hi<i>, sorts and cuts at
// limit+1 like the other seed statements.
func prefixSeedStatement(where string, i int, key string, limit int, params map[string]string) string {
	lo, hi, hasHi := prefixRange(key, nameKeyIndexBytes)
	params[fmt.Sprintf("lo%d", i)] = lo
	bounds := fmt.Sprintf(" AND %s.name_key >= $lo%d", entityTag, i)
	if hasHi {
		params[fmt.Sprintf("hi%d", i)] = hi
		bounds += fmt.Sprintf(" AND %s.name_key < $hi%d", entityTag, i)
	}
	return "LOOKUP ON " + entityTag + " WHERE " + where + bounds + candidateYield() +
		" | YIELD $-.vid AS vid,$-.doc AS doc,$-.name AS name,$-.name_key AS name_key" +
		fmt.Sprintf(" WHERE $-.name_key STARTS WITH $q%d", i) +
		" | ORDER BY $-.name_key,$-.doc,$-.vid | LIMIT " + fmt.Sprint(limit+1) + ";"
}

func candidateYield() string {
	return " YIELD id(vertex) AS vid," + entityTag + ".doc AS doc," + entityTag + ".name AS name," +
		entityTag + ".name_key AS name_key"
}

// seedRows runs one seed statement and returns at most limit rows of
// candidateColumns, reporting whether the store had more and whether the
// statement reached the store at all.
func (s *searchRun) seedRows(ctx context.Context, kind, stmt string, params map[string]string, want []string,
	limit int,
) (rows [][]string, more, sent bool, err error) {
	if err := s.r.check(ctx); err != nil {
		return nil, false, false, err
	}
	b := batch{Kind: kind, Statement: stmt, Params: params}
	if b.size() > s.r.c.cfg.StatementBytes {
		return nil, false, false, ErrBudget
	}
	o := s.r.c.execute(ctx, b, false)
	if o.Err != nil {
		return nil, false, o.Sent, o.Err
	}
	if err := columns(o.Table, want); err != nil {
		return nil, false, true, err
	}
	rows = o.Table.Rows
	if len(rows) > limit {
		rows, more = rows[:limit], true
	}
	for i, row := range rows {
		if len(row) != len(want) {
			return nil, false, true, ErrInvalid
		}
		rows[i] = row[:len(candidateColumns)]
	}
	return rows, more, true, nil
}

// candidates returns ranked name matches: exact (normalised) matches first,
// then shorter names, then by name, document and identity.
//
// Matching is evaluated inside NebulaGraph and only matching rows come back:
//
//	(a) exact:    name_key IN [...] through the name_key index, so an exact
//	              match is never lost to truncation of (b);
//	(b) contains: the namespace's entities filtered by CONTAINS in the
//	              query pipe. No index serves substring matching, so this is
//	              a scan inside the store, bounded by SearchScanTimeout.
//
// When (b) cannot finish in time the namespace is remembered as large and
// (b) is replaced by prefix matching through an index range scan
// (IndexOnly).
//
// Every statement sorts before LIMIT so truncation is deterministic, and too
// many matches truncate (CandidateTruncated) instead of failing the search.
// The in-store sort uses byte length where the final ranking below uses rune
// count; they only differ in which rows survive a truncation.
func (s *searchRun) candidates(ctx context.Context) ([]seedCandidate, error) {
	cfg := s.r.c.cfg
	limit := cfg.SnapshotRecords
	where, scope := scopeWhere(entityTag, s.ns)
	params := map[string]string{}
	for name, v := range scope {
		params[name] = v
	}
	refs := make([]string, len(s.keys))
	contains := make([]string, len(s.keys))
	for i, k := range s.keys {
		name := fmt.Sprintf("q%d", i)
		params[name] = k
		refs[i] = "$" + name
		contains[i] = "$-.name_key CONTAINS $" + name
	}
	cut := " | LIMIT " + fmt.Sprint(limit+1) + ";"
	exact := "LOOKUP ON " + entityTag + " WHERE " + where + " AND " + entityTag + ".name_key IN [" +
		strings.Join(refs, ",") + "]" + candidateYield() + " | ORDER BY $-.doc,$-.vid" + cut
	rows, more, _, err := s.seedRows(ctx, "seed_exact", exact, params, candidateColumns, limit)
	if err != nil {
		return nil, err
	}
	s.d.CandidateTruncated = more

	large := s.isLargeScope()
	if !large {
		filter := "LOOKUP ON " + entityTag + " WHERE " + where + candidateYield() +
			" | YIELD $-.vid AS vid,$-.doc AS doc,$-.name AS name,$-.name_key AS name_key," +
			"length($-.name) AS len WHERE " + strings.Join(contains, " OR ") +
			" | ORDER BY $-.len,$-.name,$-.doc,$-.vid" + cut
		scanCtx, cancel := context.WithTimeout(ctx, cfg.SearchScanTimeout)
		found, more, sent, err := s.seedRows(scanCtx, "seed_filter", filter, params,
			append(append([]string(nil), candidateColumns...), "len"), limit)
		// The scan's budget is over, the caller's is not.
		expired := (scanCtx.Err() != nil || deadlinePassed(scanCtx)) && ctx.Err() == nil && !deadlinePassed(ctx)
		cancel()
		switch {
		case err == nil:
			rows = append(rows, found...)
			s.d.CandidateTruncated = s.d.CandidateTruncated || more
		case expired:
			// The scan did not fit its budget. Only a scan that actually ran
			// marks the namespace; time lost waiting for a connection does not.
			large = true
			if sent {
				s.markLargeScope()
			}
		default:
			return nil, err
		}
	}
	if large {
		s.d.IndexOnly = true
		for i, k := range s.keys {
			stmt := prefixSeedStatement(where, i, k, limit, params)
			found, more, _, err := s.seedRows(ctx, fmt.Sprintf("seed_index_%d", i), stmt, params,
				candidateColumns, limit)
			if err != nil {
				return nil, err
			}
			rows = append(rows, found...)
			s.d.CandidateTruncated = s.d.CandidateTruncated || more
			if len(rows) > 2*limit && i < len(s.keys)-1 {
				// Enough candidates from earlier names; later names are cut.
				s.d.CandidateTruncated = true
				break
			}
		}
	}
	if err := s.account(rows); err != nil {
		return nil, err
	}
	match := func(key string) bool {
		for _, k := range s.keys {
			if strings.Contains(key, k) {
				return true
			}
		}
		return false
	}
	var out []seedCandidate
	seen := map[string]bool{}
	for _, row := range rows {
		vid, doc, name, key := row[0], row[1], row[2], row[3]
		if seen[vid] || !match(key) {
			continue
		}
		seen[vid] = true
		// The VID is a digest of (kb, doc, name): recomputing it proves the
		// row belongs to this namespace and that name and name_key are intact.
		want, err := identity(cfg, "entity", types.NameSpace{KnowledgeBase: s.ns.KnowledgeBase, Knowledge: doc}, name)
		if err != nil || doc == "" || (s.ns.Knowledge != "" && doc != s.ns.Knowledge) || want != vid ||
			nameKey(name) != key {
			return nil, fmt.Errorf("%w: seed identity", ErrInvalid)
		}
		exact := false
		for _, k := range s.keys {
			exact = exact || key == k
		}
		out = append(out, seedCandidate{id: vid, name: name, doc: doc, exact: exact})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.exact != b.exact {
			return a.exact
		}
		if na, nb := utf8.RuneCountInString(a.name), utf8.RuneCountInString(b.name); na != nb {
			return na < nb
		}
		if a.name != b.name {
			return a.name < b.name
		}
		if a.doc != b.doc {
			return a.doc < b.doc
		}
		return a.id < b.id
	})
	return out, nil
}

// connectedSeeds expands candidates in rank order, keeping only those with at
// least one relation, until searchMaxSeeds are selected.
func (s *searchRun) connectedSeeds(ctx context.Context, cands []seedCandidate) ([]seedCandidate,
	map[string][]string, error,
) {
	relsOf := map[string][]string{}
	var selected []seedCandidate
	const step = 64
	for start := 0; start < len(cands); start += step {
		chunk := cands[start:min(start+step, len(cands))]
		want := map[string]bool{}
		ids := make([]string, len(chunk))
		for i, c := range chunk {
			ids[i] = c.id
			want[c.id] = true
		}
		rows, err := s.r.readMany(ctx, "seed_expand", "GO FROM ",
			" OVER primind_from,primind_to REVERSELY YIELD dst(edge) AS ent,src(edge) AS rel;",
			ids, []string{"ent", "rel"})
		if err != nil {
			return nil, nil, err
		}
		if err := s.account(rows); err != nil {
			return nil, nil, err
		}
		for _, row := range rows {
			if !want[row[0]] || !safeVID(row[1]) {
				return nil, nil, fmt.Errorf("%w: expansion row", ErrInvalid)
			}
			relsOf[row[0]] = append(relsOf[row[0]], row[1])
		}
		for _, c := range chunk {
			if len(relsOf[c.id]) == 0 {
				continue
			}
			s.d.ConnectedSeeds++
			if len(selected) == searchMaxSeeds {
				s.d.SeedTruncated = true
				continue
			}
			selected = append(selected, c)
		}
		if s.d.SeedTruncated {
			break
		}
	}
	for id := range relsOf {
		sort.Strings(relsOf[id])
		relsOf[id] = dedupSorted(relsOf[id])
	}
	s.d.SelectedSeeds = len(selected)
	return selected, relsOf, nil
}

func dedupSorted(xs []string) []string {
	out := xs[:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// relationsAndEntities reads, in one traversal from the relation vertices,
// both endpoint edges together with the relation's own properties ($^) and
// each endpoint entity's properties ($$), then verifies all of them.
func (s *searchRun) relationsAndEntities(ctx context.Context, ids []string) (map[string]*types.GraphRelation,
	map[string]*types.GraphNode, error,
) {
	cfg := s.r.c.cfg
	want := []string{"rel", "kind", "ent", "rank"}
	yield := " OVER primind_from,primind_to YIELD src(edge) AS rel,type(edge) AS kind,dst(edge) AS ent," +
		"rank(edge) AS rank"
	for _, f := range tagFields[relationTag] {
		yield += ",$^." + relationTag + "." + f + " AS r_" + f
		want = append(want, "r_"+f)
	}
	for _, f := range tagFields[entityTag] {
		yield += ",$$." + entityTag + "." + f + " AS e_" + f
		want = append(want, "e_"+f)
	}
	rows, err := s.r.readMany(ctx, "search_relation", "GO FROM ", yield+";", ids, want)
	if err != nil {
		return nil, nil, err
	}
	if err := s.account(rows); err != nil {
		return nil, nil, err
	}
	nr := len(tagFields[relationTag])
	rels := map[string]*types.GraphRelation{}
	relFields := map[string]string{}
	nodes := map[string]*types.GraphNode{}
	got := map[string]int{}
	for _, row := range rows {
		id, kind, ent := row[0], row[1], row[2]
		rf, ef := row[4:4+nr], row[4+nr:]
		key := strings.Join(rf, "\x00")
		rel := rels[id]
		if rel == nil {
			if err := verifyRecord(cfg, relationTag, s.ns, id, rf); err != nil {
				return nil, nil, err
			}
			n := types.NameSpace{KnowledgeBase: rf[0], Knowledge: rf[1]}
			src, _ := identity(cfg, "entity", n, rf[2])
			dst, _ := identity(cfg, "entity", n, rf[3])
			rel = &types.GraphRelation{ID: id, SourceID: src, TargetID: dst, Node1: rf[2], Node2: rf[3], Type: rf[4]}
			rels[id] = rel
			relFields[id] = key
		} else if relFields[id] != key {
			return nil, nil, fmt.Errorf("%w: relation record", ErrInvalid)
		}
		if row[3] != "0" {
			return nil, nil, fmt.Errorf("%w: relation edge rank", ErrInvalid)
		}
		switch {
		case kind == "primind_from" && ent == rel.SourceID:
			got[id] |= 1
		case kind == "primind_to" && ent == rel.TargetID:
			got[id] |= 2
		default:
			return nil, nil, fmt.Errorf("%w: relation edge mismatch", ErrInvalid)
		}
		if nodes[ent] == nil {
			rec := storedRecord{ID: ent, Tag: entityTag, Fields: ef}
			if err := verifyRecord(cfg, entityTag, s.ns, ent, ef); err != nil {
				return nil, nil, err
			}
			node, err := entityNode(rec)
			if err != nil {
				return nil, nil, err
			}
			nodes[ent] = node
		}
	}
	for _, id := range ids {
		if got[id] != 3 {
			return nil, nil, fmt.Errorf("%w: missing relation endpoint edge", ErrInvalid)
		}
	}
	return rels, nodes, nil
}

// sources reads each endpoint's owner edges together with the source
// vertex properties ($$ is the vertex reached, i.e. the source) and verifies them.
func (s *searchRun) sources(ctx context.Context, entityIDs []string, nodes map[string]*types.GraphNode) error {
	cfg := s.r.c.cfg
	want := []string{"src", "ent"}
	yield := " OVER primind_owner REVERSELY YIELD src(edge) AS src,dst(edge) AS ent"
	for _, f := range tagFields[sourceTag] {
		yield += ",$$." + sourceTag + "." + f + " AS s_" + f
		want = append(want, "s_"+f)
	}
	rows, err := s.r.readMany(ctx, "search_source", "GO FROM ", yield+";", entityIDs, want)
	if err != nil {
		return err
	}
	if err := s.account(rows); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, row := range rows {
		src, ent, fields := row[0], row[1], row[2:]
		if nodes[ent] == nil || seen[src] {
			return fmt.Errorf("%w: source owner", ErrInvalid)
		}
		seen[src] = true
		if err := verifyRecord(cfg, sourceTag, s.ns, src, fields); err != nil {
			return err
		}
		owner, _ := identity(cfg, "entity", types.NameSpace{KnowledgeBase: fields[0], Knowledge: fields[1]}, fields[2])
		if owner != ent {
			return fmt.Errorf("%w: source owner mismatch", ErrInvalid)
		}
		nodes[ent].Chunks = append(nodes[ent].Chunks, fields[3])
	}
	for _, n := range nodes {
		sort.Strings(n.Chunks)
	}
	return nil
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
		owned := make(map[string]bool, len(rows))
		for _, row := range rows {
			owned[row.ID] = true
		}
		if err := r.foreignEndpoints(ctx, ns, owned, edges); err != nil {
			return err
		}
		for _, kind := range edgeNames {
			var records []record
			for _, edge := range edges {
				if edge.Kind == kind {
					records = append(records, record{
						ID:    edge.Source + "->" + edge.Target,
						Value: literal(edge.Source) + "->" + literal(edge.Target),
					})
				}
			}
			if err := appendBatch(r.c.cfg, &plan, "delete_edge", "DELETE EDGE "+kind+" ", ";", records); err != nil {
				return err
			}
		}
		var records []record
		for _, row := range rows {
			records = append(records, record{ID: row.ID, Value: literal(row.ID)})
		}
		if err := appendBatch(r.c.cfg, &plan, "delete", "DELETE VERTEX ", " WITH EDGE;", records); err != nil {
			return err
		}
	}
	return r.runPlan(ctx, "DelGraph", plan)
}
