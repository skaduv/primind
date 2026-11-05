package nebula_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	neo4jRepo "github.com/primind/primind/internal/application/repository/retriever/neo4j"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

// TestNeo4jRealContract runs the NG-01 fixture contract (Q1–Q10, S1, R1, C1,
// D1, D2) against a live Neo4j through the formal RetrieveGraphRepository
// methods, so both graph backends are checked against the same golden sets.
// Every namespace is prefixed with a run id and only those labels are removed.
// Needs PRIMIND_NEO4J_TEST_URI, PRIMIND_NEO4J_TEST_USER (default neo4j) and
// PRIMIND_NEO4J_TEST_PASSWORD; skipped otherwise.
func TestNeo4jRealContract(t *testing.T) {
	uri, password := os.Getenv("PRIMIND_NEO4J_TEST_URI"), os.Getenv("PRIMIND_NEO4J_TEST_PASSWORD")
	if uri == "" || password == "" {
		t.Skip("PRIMIND_NEO4J_TEST_URI / PRIMIND_NEO4J_TEST_PASSWORD not set")
	}
	user := os.Getenv("PRIMIND_NEO4J_TEST_USER")
	if user == "" {
		user = "neo4j"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	newDriver := func() neo4j.Driver {
		d, err := neo4j.NewDriver(uri, neo4j.BasicAuth(user, password, ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.VerifyAuthentication(ctx, nil); err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		return d
	}
	driver := newDriver()
	info, err := driver.GetServerInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("server agent=%s protocol=%v", info.Agent(), info.ProtocolVersion())
	if _, err := neo4j.ExecuteQuery(ctx, driver, "RETURN apoc.version() AS v", nil,
		neo4j.EagerResultTransformer); err != nil {
		t.Fatalf("APOC is required by the Neo4j repository: %v", err)
	}

	run := fmt.Sprintf("n4j%d", time.Now().UnixNano()%1e9)
	impl := neo4jRepo.NewNeo4jRepository(driver).(*neo4jRepo.Neo4jRepository)
	var repo interfaces.RetrieveGraphRepository = impl
	kbLabels := []string{
		impl.Label(mappedNS(run, types.NameSpace{KnowledgeBase: "kb-a"})),
		impl.Label(mappedNS(run, types.NameSpace{KnowledgeBase: "kb-b"})),
	}
	t.Cleanup(func() {
		for _, label := range kbLabels {
			if _, err := neo4j.ExecuteQuery(context.Background(), driver, "MATCH (n:"+label+") DETACH DELETE n",
				nil, neo4j.EagerResultTransformer); err != nil {
				t.Errorf("cleanup %s: %v", label, err)
			}
		}
	})
	snapshot := func() (fixtureState, error) { return neo4jSnapshotAll(ctx, driver, impl, run) }

	in, golden := loadFixtures(t)
	checks := map[string]fixtureCheck{}
	for _, q := range golden.Checks {
		checks[q.ID] = q
	}
	addAll := func() {
		for _, c := range in.Cases {
			call := mappedCall(run, c)
			if err := repo.AddGraph(ctx, call.Namespace, call.Graphs); err != nil {
				t.Fatalf("AddGraph %s: %v", c.ID, err)
			}
		}
	}
	addAll()
	base, err := snapshot()
	if err != nil || compareState(base, mappedState(run, golden.Baseline)) != nil {
		t.Fatalf("baseline: err=%v diff=%v", err, compareState(base, mappedState(run, golden.Baseline)))
	}
	if len(base.Entities) == 0 || len(base.Relations) == 0 {
		t.Fatal("baseline snapshot is empty; the contract would pass vacuously")
	}
	t.Logf("baseline: %d entities, %d relations, %d connected", len(base.Entities), len(base.Relations), base.Connected)

	searchState := func(ns types.NameSpace, names []string) (*types.GraphData, fixtureState, error) {
		g, err := repo.SearchNode(ctx, ns, names)
		if err != nil {
			return nil, fixtureState{}, err
		}
		if g == nil {
			g = &types.GraphData{}
		}
		s, err := graphState(g, ns.KnowledgeBase)
		return g, s, err
	}
	for _, id := range []string{"Q1", "Q2", "Q3", "Q4", "Q5", "Q6", "Q7", "Q8", "Q9", "Q10"} {
		q := checks[id]
		ns := mappedNS(run, *q.Namespace)
		g, s, err := searchState(ns, q.QueryNames)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if err := compareState(s, mappedState(run, q.Expected)); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		t.Logf("%s: %d entities, %d relations", id, len(s.Entities), len(s.Relations))
		for _, n := range g.Node {
			for _, c := range n.Chunks {
				for _, forbidden := range q.ForbiddenChunks {
					if c == run+"-"+forbidden {
						t.Errorf("%s: forbidden chunk %s returned", id, forbidden)
					}
				}
			}
		}
		if q.First != nil && (len(g.Node) == 0 || g.Node[0].Name != q.First.Name) {
			t.Errorf("%s: first node is not %s", id, q.First.Name)
		}
	}

	// S1: the source list of the base entity is exactly the declared chunks.
	s1 := mappedState(run, checks["S1"].Expected)
	_, q1, err := searchState(mappedNS(run, a1), []string{checks["Q1"].QueryNames[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := compareState(fixtureState{
		Entities: s1.Entities, Relations: []relationKey{},
		Nodes: nodesFor(q1, s1.Entities),
	}, s1); err != nil {
		t.Errorf("S1: %v", err)
	}

	// R1: replaying the same input is idempotent.
	addAll()
	if s, err := snapshot(); err != nil || compareState(s, mappedState(run, checks["R1"].Expected)) != nil {
		t.Errorf("R1: err=%v diff=%v", err, compareState(s, mappedState(run, checks["R1"].Expected)))
	}

	// C1: two independent clients append sources to the same entity concurrently.
	var wg sync.WaitGroup
	errs := make([]error, len(in.Supplemental))
	for i, sup := range in.Supplemental {
		wg.Add(1)
		go func(i int, sup fixtureCall) {
			defer wg.Done()
			call := mappedCall(run, sup)
			r := neo4jRepo.NewNeo4jRepository(newDriver())
			errs[i] = r.AddGraph(ctx, call.Namespace, call.Graphs)
		}(i, sup)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("C1 writer %d: %v", i, e)
		}
	}
	if s, err := snapshot(); err != nil || compareState(s, mappedState(run, checks["C1"].Expected)) != nil {
		t.Errorf("C1: err=%v diff=%v", err, compareState(s, mappedState(run, checks["C1"].Expected)))
	}

	// D1/D2: scoped deletion (repeated namespace is harmless), then the rest.
	for _, id := range []string{"D1", "D2"} {
		q := checks[id]
		var nss []types.NameSpace
		for _, ns := range q.DeleteNamespaces {
			nss = append(nss, mappedNS(run, ns))
		}
		if err := repo.DelGraph(ctx, nss); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if s, err := snapshot(); err != nil || compareState(s, mappedState(run, q.Expected)) != nil {
			t.Errorf("%s: err=%v diff=%v", id, err, compareState(s, mappedState(run, q.Expected)))
		}
		for _, qid := range q.EmptyQueries {
			qq := checks[qid]
			if _, s, err := searchState(mappedNS(run, *qq.Namespace),
				qq.QueryNames); err != nil || len(s.Entities) != 0 {
				t.Errorf("%s: %s not empty after delete (err=%v)", id, qid, err)
			}
		}
	}
	t.Logf("run=%s contract checks Q1–Q10,S1,R1,C1,D1,D2 executed", run)
}

func nodesFor(s fixtureState, keys []entityKey) []fixtureNode {
	out := []fixtureNode{}
	for _, k := range keys {
		for _, n := range s.Nodes {
			if n.Key == k {
				out = append(out, n)
			}
		}
	}
	return out
}

// neo4jSnapshotAll reads every entity and relation stored under the run's two
// KB labels, the Neo4j counterpart of the Nebula adapter's Snapshot.
func neo4jSnapshotAll(ctx context.Context, driver neo4j.Driver, repo *neo4jRepo.Neo4jRepository,
	run string,
) (fixtureState, error) {
	all := fixtureState{Entities: []entityKey{}, Relations: []relationKey{}, Nodes: []fixtureNode{}}
	for _, kb := range []string{"kb-a", "kb-b"} {
		ns := mappedNS(run, types.NameSpace{KnowledgeBase: kb})
		label := repo.Label(ns)
		res, err := neo4j.ExecuteQuery(ctx, driver,
			"MATCH (n:"+label+") OPTIONAL MATCH (n)-[r]->(m:"+label+") RETURN n, r, m",
			nil, neo4j.EagerResultTransformer)
		if err != nil {
			return all, err
		}
		g := &types.GraphData{}
		seen, rels := map[string]bool{}, map[string]bool{}
		names := map[string]string{}
		for _, rec := range res.Records {
			n, _, _ := neo4j.GetRecordValue[neo4j.Node](rec, "n")
			if !seen[n.ElementId] {
				seen[n.ElementId] = true
				names[n.ElementId], _ = n.Props["name"].(string)
				g.Node = append(g.Node, snapshotNode(n))
			}
			r, isNil, _ := neo4j.GetRecordValue[neo4j.Relationship](rec, "r")
			if isNil || rels[r.ElementId] {
				continue
			}
			rels[r.ElementId] = true
			m, _, _ := neo4j.GetRecordValue[neo4j.Node](rec, "m")
			if !seen[m.ElementId] {
				seen[m.ElementId] = true
				names[m.ElementId], _ = m.Props["name"].(string)
				g.Node = append(g.Node, snapshotNode(m))
			}
			g.Relation = append(g.Relation, &types.GraphRelation{
				Node1: names[r.StartElementId], Node2: names[r.EndElementId], Type: r.Type,
				ID: r.ElementId, SourceID: r.StartElementId, TargetID: r.EndElementId,
			})
		}
		s, err := graphState(g, ns.KnowledgeBase)
		if err != nil {
			return all, err
		}
		all.Entities = append(all.Entities, s.Entities...)
		all.Relations = append(all.Relations, s.Relations...)
		all.Nodes = append(all.Nodes, s.Nodes...)
		all.Connected += s.Connected
	}
	return all, nil
}

func snapshotNode(n neo4j.Node) *types.GraphNode {
	name, _ := n.Props["name"].(string)
	kg, _ := n.Props["kg"].(string)
	str := func(v any) []string {
		list, _ := v.([]any)
		out := make([]string, 0, len(list))
		for _, x := range list {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return &types.GraphNode{
		Name: name, Chunks: str(n.Props["chunks"]), Attributes: str(n.Props["attributes"]),
		ID: n.ElementId, KnowledgeID: kg,
	}
}
