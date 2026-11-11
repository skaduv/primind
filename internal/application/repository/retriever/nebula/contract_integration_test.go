package nebula_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/nebula"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

func readJSONFile(path string, target any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.Size() > 16<<20 {
		return fmt.Errorf("bounded JSON file required")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 16<<20 {
		return fmt.Errorf("bounded JSON file required")
	}
	return strictDecode(data, target)
}

func writeEvidence(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

func loadRun(t *testing.T, performance bool) (adapter.RunProfile, adapter.Config, string) {
	t.Helper()
	var p adapter.RunProfile
	if os.Getenv("PRIMIND_NG_E2_ENABLE") != "authorized-isolated-run" {
		t.Skip("BLOCKED/NOT_RUN: explicit isolated NebulaGraph authorization absent; SKIP is not E2 PASS")
	}
	if err := readJSONFile(os.Getenv("PRIMIND_NG_PROFILE"), &p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(performance); err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != p.SourceCommit {
		t.Fatal("profile source commit must match checked-out HEAD")
	}
	out := os.Getenv("PRIMIND_NG_OUTPUT")
	if !filepath.IsAbs(out) {
		t.Fatal("absolute dedicated evidence directory required")
	}
	cfg := adapter.DefaultConfig()
	cfg.Space = p.Space
	cfg.Addresses = strings.Split(os.Getenv("PRIMIND_NG_ADDRESSES"), ",")
	cfg.Username = os.Getenv("PRIMIND_NG_USERNAME")
	cfg.Password = os.Getenv("PRIMIND_NG_PASSWORD")
	cfg.TLS = os.Getenv("PRIMIND_NG_TLS") == "true"
	cfg.ServerName = os.Getenv("PRIMIND_NG_SERVER_NAME")
	if ca := os.Getenv("PRIMIND_NG_CA_FILE"); ca != "" {
		b, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		cfg.CACertificatePEM = string(b)
	}
	if cfg.Username == "" || cfg.Password == "" || cfg.Addresses[0] == "" {
		t.Fatal("explicit credentials and endpoint required; no .env loading")
	}
	cfg.Connections = p.Connections
	cfg.Writes = p.MaxConcurrency
	cfg.StatementBytes = p.StatementBytes
	return p, cfg, out
}

func mappedNS(run string, n types.NameSpace) types.NameSpace {
	k := n.Knowledge
	if k != "" {
		k = run + "-" + k
	}
	return types.NameSpace{KnowledgeBase: run + "-" + n.KnowledgeBase, Knowledge: k}
}

func mappedCall(run string, c fixtureCall) fixtureCall {
	// JSON copy preserves the accepted fixed input and modifies only scoped IDs.
	b, _ := json.Marshal(c)
	var out fixtureCall
	_ = json.Unmarshal(b, &out)
	out.Namespace = mappedNS(run, c.Namespace)
	for _, g := range out.Graphs {
		for _, n := range g.Node {
			for i, s := range n.Chunks {
				n.Chunks[i] = run + "-" + s
			}
		}
	}
	return out
}

func mappedState(run string, s fixtureState) fixtureState {
	b, _ := json.Marshal(s)
	var out fixtureState
	_ = json.Unmarshal(b, &out)
	for i := range out.Entities {
		out.Entities[i].KB = run + "-" + out.Entities[i].KB
		out.Entities[i].Doc = run + "-" + out.Entities[i].Doc
	}
	for i := range out.Relations {
		out.Relations[i].KB = run + "-" + out.Relations[i].KB
		out.Relations[i].Doc = run + "-" + out.Relations[i].Doc
	}
	for i := range out.Nodes {
		n := &out.Nodes[i]
		n.Key.KB = run + "-" + n.Key.KB
		n.Key.Doc = run + "-" + n.Key.Doc
		for j, c := range n.Chunks {
			n.Chunks[j] = run + "-" + c
		}
	}
	return out
}

func graphState(g *types.GraphData, kb string) (fixtureState, error) {
	s := fixtureState{Entities: []entityKey{}, Relations: []relationKey{}, Nodes: []fixtureNode{}}
	if g == nil {
		return s, fmt.Errorf("nil graph")
	}
	nodes := map[string]entityKey{}
	connected := map[string]bool{}
	for _, n := range g.Node {
		if n == nil || n.ID == "" || n.KnowledgeID == "" {
			return s, fmt.Errorf("missing retrieval identity")
		}
		if _, exists := nodes[n.ID]; exists {
			return s, fmt.Errorf("duplicate node identity")
		}
		k := entityKey{kb, n.KnowledgeID, n.Name}
		nodes[n.ID] = k
		s.Entities = append(s.Entities, k)
		attrs := n.Attributes
		if attrs == nil {
			attrs = []string{}
		}
		chunks := n.Chunks
		if chunks == nil {
			chunks = []string{}
		}
		s.Nodes = append(s.Nodes, fixtureNode{k, chunks, attrs})
	}
	edgeIDs := map[string]bool{}
	for _, e := range g.Relation {
		if e == nil || e.ID == "" || edgeIDs[e.ID] {
			return s, fmt.Errorf("invalid relation identity")
		}
		edgeIDs[e.ID] = true
		src, ok1 := nodes[e.SourceID]
		dst, ok2 := nodes[e.TargetID]
		if !ok1 || !ok2 || src.Doc != dst.Doc || src.Name != e.Node1 || dst.Name != e.Node2 {
			return s, fmt.Errorf("direction/endpoint mismatch")
		}
		s.Relations = append(s.Relations, relationKey{kb, src.Doc, e.Node1, e.Node2, e.Type})
		connected[e.SourceID] = true
		connected[e.TargetID] = true
	}
	s.Connected = len(connected)
	return s, nil
}

func waitCheck(ctx context.Context, cfg adapter.Config, check func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, cfg.ReadyTimeout)
	defer cancel()
	var last error
	for {
		if last = check(bounded); last == nil {
			return nil
		}
		timer := time.NewTimer(cfg.PollInterval)
		select {
		case <-bounded.Done():
			timer.Stop()
			return fmt.Errorf("visibility deadline: %w; last=%v", bounded.Err(), last)
		case <-timer.C:
		}
	}
}

func TestVisibilityWaitBoundsTheActualProbe(t *testing.T) {
	cfg := adapter.DefaultConfig()
	cfg.ReadyTimeout, cfg.PollInterval = 20*time.Millisecond, time.Millisecond
	start := time.Now()
	err := waitCheck(context.Background(), cfg, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("probe deadline not enforced: %v", err)
	}
}

func snapshotAll(ctx context.Context, r *adapter.Repository, run string) (fixtureState, error) {
	all := fixtureState{Entities: []entityKey{}, Relations: []relationKey{}, Nodes: []fixtureNode{}}
	for _, kb := range []string{"kb-a", "kb-b"} {
		g, err := r.Snapshot(ctx, mappedNS(run, types.NameSpace{KnowledgeBase: kb}))
		if err != nil {
			return all, err
		}
		s, err := graphState(g, run+"-"+kb)
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

func TestNebulaRealContract(t *testing.T) {
	p, cfg, out := loadRun(t, false)
	in, golden := loadFixtures(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.MaxDurationSeconds)*time.Second)
	defer cancel()
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	var manifestCalls []adapter.GraphCall
	for _, call := range append(append([]fixtureCall{}, in.Cases...), in.Supplemental...) {
		c := mappedCall(p.RunID, call)
		manifestCalls = append(manifestCalls, adapter.GraphCall{Namespace: c.Namespace, Graphs: c.Graphs})
	}
	manifest, err := adapter.BuildManifest(cfg, p.RunID, manifestCalls)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvidence(filepath.Join(out, "run-manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if p.CreateEmptySchema {
		if err := adapter.InitializeSchema(ctx, cfg, adapter.SchemaAuthorization{
			Space:                       p.Space,
			CreateInEmptyDedicatedSpace: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := adapter.NewRepository(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	evidence, err := r.InspectService(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvidence(filepath.Join(out, "service-versions.json"), evidence); err != nil {
		t.Fatal(err)
	}
	residue, err := r.ProbeManifest(ctx, manifest)
	if err != nil || len(residue.Vertices)+len(residue.Edges) != 0 {
		t.Fatalf("run is not empty: %v", err)
	}
	var repo interfaces.RetrieveGraphRepository = r
	results := map[string]string{}
	observations := map[string]any{}
	compareObserved := func(id string, actual, expected fixtureState) error {
		err := compareState(actual, expected)
		observations[id] = map[string]any{"actual": actual, "expected": expected, "equal": err == nil}
		return err
	}
	t.Cleanup(func() {
		if err := writeEvidence(filepath.Join(out, "contract-checks.json"), map[string]any{
			"checks":           results,
			"metrics":          r.Metrics(),
			"remaining_status": "consult residue evidence; no automatic cleanup after failure",
		}); err != nil {
			t.Error(err)
		}
		if err := writeEvidence(filepath.Join(out, "expected-actual.json"), observations); err != nil {
			t.Error(err)
		}
	})
	// B-11 precedes the full fixture run: only the two A1 calls, Q3/Q4,
	// repeated scoped deletion, and both snapshot and planned-ID residue checks.
	for _, c := range in.Cases {
		if c.Namespace != a1 {
			continue
		}
		call := mappedCall(p.RunID, c)
		if err := repo.AddGraph(ctx, call.Namespace, call.Graphs); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range golden.Checks {
		if q.ID != "Q3" && q.ID != "Q4" {
			continue
		}
		if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
			ns := mappedNS(p.RunID, *q.Namespace)
			g, e := repo.SearchNode(ctx, ns, q.QueryNames)
			if e != nil {
				return e
			}
			s, e := graphState(g, ns.KnowledgeBase)
			if e != nil {
				return e
			}
			return compareObserved("B-11-"+q.ID, s, mappedState(p.RunID, q.Expected))
		}); err != nil {
			t.Fatal(err)
		}
	}
	for twice := 0; twice < 2; twice++ {
		if err := repo.DelGraph(ctx, []types.NameSpace{mappedNS(p.RunID, a1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
		g, e := r.Snapshot(ctx, mappedNS(p.RunID, a1))
		if e != nil {
			return e
		}
		if len(g.Node)+len(g.Relation) != 0 {
			return fmt.Errorf("B-11 scope residue")
		}
		left, e := r.ProbeManifest(ctx, manifest)
		if e != nil {
			return e
		}
		if len(left.Vertices)+len(left.Edges) != 0 {
			return fmt.Errorf("B-11 manifest residue")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	results["B-11"] = "PASS"
	for _, c := range in.Cases {
		call := mappedCall(p.RunID, c)
		if err := repo.AddGraph(ctx, call.Namespace, call.Graphs); err != nil {
			t.Fatalf("write %s: %v", c.ID, err)
		}
	}
	for _, q := range golden.Checks {
		if !strings.HasPrefix(q.ID, "Q") {
			continue
		}
		actualNS := mappedNS(p.RunID, *q.Namespace)
		err := waitCheck(ctx, cfg, func(ctx context.Context) error {
			got, err := repo.SearchNode(ctx, actualNS, q.QueryNames)
			if err != nil {
				return err
			}
			state, err := graphState(got, actualNS.KnowledgeBase)
			if err != nil {
				return err
			}
			if err := compareObserved(q.ID, state, mappedState(p.RunID, q.Expected)); err != nil {
				return err
			}
			if q.First != nil && (len(got.Node) == 0 || got.Node[0].Name != q.First.Name ||
				got.Node[0].KnowledgeID != p.RunID+"-"+q.First.Doc) {
				return fmt.Errorf("raw seed first mismatch")
			}
			return nil
		})
		if err != nil {
			results[q.ID] = "FAIL"
			t.Fatal(err)
		}
		results[q.ID] = "PASS"
		queryStart := time.Now()
		_, diag, diagErr := r.Search(ctx, actualNS, q.QueryNames)
		if diagErr != nil {
			t.Fatal(diagErr)
		}
		if err := writeEvidence(filepath.Join(out, q.ID+"-query.json"), map[string]any{
			"diagnostics": diag,
			"elapsed_ns":  int64(time.Since(queryStart)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	unknown, err := repo.SearchNode(ctx, mappedNS(p.RunID, types.NameSpace{KnowledgeBase: "unknown"}), []string{"主泵A"})
	if err != nil || unknown == nil || len(unknown.Node)+len(unknown.Relation) != 0 {
		t.Fatalf("valid unknown namespace: %v", err)
	}
	results["unknown-kb"] = "PASS"
	plans, err := r.ScopePlans(ctx, mappedNS(p.RunID, a1))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvidence(filepath.Join(out, "query-plans.json"), plans); err != nil {
		t.Fatal(err)
	}
	// Observe the original two-source state before replay can repair a missed
	// initial write. This full baseline comparison includes the S1 source check.
	if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
		s, e := snapshotAll(ctx, r, p.RunID)
		if e != nil {
			return e
		}
		return compareObserved("S1", s, mappedState(p.RunID, golden.Baseline))
	}); err != nil {
		t.Fatal(err)
	}
	results["S1"] = "PASS"
	for _, c := range in.Cases {
		call := mappedCall(p.RunID, c)
		if err := repo.AddGraph(ctx, call.Namespace, call.Graphs); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
		s, err := snapshotAll(ctx, r, p.RunID)
		if err != nil {
			return err
		}
		return compareObserved("R1", s, mappedState(p.RunID, golden.Baseline))
	}); err != nil {
		t.Fatal(err)
	}
	results["R1"] = "PASS"
	// Every round starts with the original two-source A1 state, so later rounds
	// exercise fresh appends rather than only replaying already-present records.
	var c1 fixtureState
	for _, q := range golden.Checks {
		if q.ID == "C1" {
			c1 = q.Expected
		}
	}
	for round := 0; round < p.C1Rounds; round++ {
		if round > 0 {
			scope := mappedNS(p.RunID, a1)
			if err := repo.DelGraph(ctx, []types.NameSpace{scope}); err != nil {
				t.Fatal(err)
			}
			if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
				g, e := r.Snapshot(ctx, scope)
				if e != nil {
					return e
				}
				if len(g.Node)+len(g.Relation) > 0 {
					return fmt.Errorf("C1 reset not yet visible")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, c := range in.Cases {
				if c.Namespace == a1 {
					call := mappedCall(p.RunID, c)
					if err := repo.AddGraph(ctx, call.Namespace, call.Graphs); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
				s, e := snapshotAll(ctx, r, p.RunID)
				if e != nil {
					return e
				}
				return compareObserved(fmt.Sprintf("C1-%02d-reset", round), s, mappedState(p.RunID, golden.Baseline))
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := runC1(ctx, out, round); err != nil {
			results["C1"] = "FAIL"
			t.Fatal(err)
		}
		var actual fixtureState
		if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
			var e error
			actual, e = snapshotAll(ctx, r, p.RunID)
			if e != nil {
				return e
			}
			return compareObserved(fmt.Sprintf("C1-%02d", round), actual, mappedState(p.RunID, c1))
		}); err != nil {
			results["C1"] = "FAIL"
			t.Fatal(err)
		}
		if err := writeEvidence(filepath.Join(out, fmt.Sprintf("c1-%02d-actual.json", round)), actual); err != nil {
			t.Fatal(err)
		}
	}

	results["C1"] = "PASS"
	for _, id := range []string{"D1", "D2"} {
		var q fixtureCheck
		for _, candidate := range golden.Checks {
			if candidate.ID == id {
				q = candidate
			}
		}
		nss := []types.NameSpace{}
		for _, ns := range q.DeleteNamespaces {
			nss = append(nss, mappedNS(p.RunID, ns))
		}
		for twice := 0; twice < 2; twice++ {
			if err := repo.DelGraph(ctx, nss); err != nil {
				results[id] = "FAIL"
				t.Fatal(err)
			}
		}
		if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
			s, err := snapshotAll(ctx, r, p.RunID)
			if err != nil {
				return err
			}
			return compareObserved(id, s, mappedState(p.RunID, q.Expected))
		}); err != nil {
			results[id] = "FAIL"
			t.Fatal(err)
		}
		for _, qid := range q.EmptyQueries {
			for _, check := range golden.Checks {
				if check.ID == qid {
					graph, err := repo.SearchNode(ctx, mappedNS(p.RunID, *check.Namespace), check.QueryNames)
					if err != nil || graph == nil || len(graph.Node)+len(graph.Relation) != 0 {
						t.Fatalf("delete %s query %s: %v", id, qid, err)
					}
				}
			}
		}
		results[id] = "PASS"
	}
	if err := waitCheck(ctx, cfg, func(ctx context.Context) error {
		var e error
		residue, e = r.ProbeManifest(ctx, manifest)
		if e != nil {
			return e
		}
		if len(residue.Vertices)+len(residue.Edges) > 0 {
			return fmt.Errorf("manifest residue")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeEvidence(filepath.Join(out, "residue.json"), residue); err != nil {
		t.Fatal(err)
	}
	results["manifest-no-residue"] = "PASS"
}

func runC1(ctx context.Context, out string, round int) (runErr error) {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	childCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	prefix := filepath.Join(out, fmt.Sprintf("c1-%02d", round))
	commands := make([]*exec.Cmd, 2)
	finished := make(chan error, 2)
	started, reaped := 0, 0
	defer func() {
		cancel()
		for _, cmd := range commands {
			if cmd != nil && cmd.Process != nil && runErr != nil {
				_ = cmd.Process.Kill()
			}
		}
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for reaped < started {
			select {
			case <-finished:
				reaped++
			case <-deadline.C:
				runErr = fmt.Errorf("C1 child drain timed out; earlier error: %v", runErr)
				return
			}
		}
	}()
	for i := range commands {
		cmd := exec.CommandContext(childCtx, binary, "-test.run=^TestNebulaC1Worker$", "-test.v")
		cmd.Env = append(os.Environ(), "PRIMIND_NG_C1_WORKER="+strconv.Itoa(i), "PRIMIND_NG_C1_BARRIER="+prefix)
		commands[i] = cmd
		log, err := os.OpenFile(prefix+fmt.Sprintf("-%d.log", i), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		cmd.Stdout = log
		cmd.Stderr = log
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			return err
		}
		started++
		go func() { e := cmd.Wait(); _ = log.Close(); finished <- e }()
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	ready := func() bool {
		_, a := os.Stat(prefix + "-0.ready")
		_, b := os.Stat(prefix + "-1.ready")
		return a == nil && b == nil
	}
	for !ready() {
		select {
		case e := <-finished:
			reaped++
			return fmt.Errorf("C1 child exited before barrier: %v", e)
		case <-childCtx.Done():
			return childCtx.Err()
		case <-deadline.C:
			return fmt.Errorf("C1 readiness timeout")
		case <-tick.C:
		}
	}
	if err := writeEvidence(prefix+".start", map[string]bool{"start": true}); err != nil {
		return err
	}
	for reaped < started {
		select {
		case err := <-finished:
			reaped++
			if err != nil {
				return err
			}
		case <-childCtx.Done():
			return childCtx.Err()
		}
	}
	return nil
}

func TestNebulaC1Worker(t *testing.T) {
	if os.Getenv("PRIMIND_NG_C1_WORKER") == "" {
		t.Skip("NOT_RUN: worker only launched by authorized C1 coordinator")
	}
	p, cfg, _ := loadRun(t, false)
	idx, err := strconv.Atoi(os.Getenv("PRIMIND_NG_C1_WORKER"))
	if err != nil || idx < 0 || idx > 1 {
		t.Fatal("invalid worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := adapter.NewRepository(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	barrier := os.Getenv("PRIMIND_NG_C1_BARRIER")
	if !filepath.IsAbs(barrier) {
		t.Fatal("absolute barrier required")
	}
	if err := writeEvidence(barrier+fmt.Sprintf("-%d.ready", idx), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(barrier + ".start"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	in, _ := loadFixtures(t)
	c := mappedCall(p.RunID, in.Supplemental[idx])
	if err := r.AddGraph(ctx, c.Namespace, c.Graphs); err != nil {
		t.Fatal(err)
	}
}

func TestRunMappingPreservesFixedSemantics(t *testing.T) {
	in, out := loadFixtures(t)
	c := mappedCall("run01", in.Cases[0])
	if c.Graphs[0].Node[4].Name != specialName || c.Graphs[0].Relation[5].Type != specialType {
		t.Fatal("special value changed")
	}
	if reflect.DeepEqual(in.Cases[0], c) || in.Cases[0].Namespace.KnowledgeBase != "kb-a" {
		t.Fatal("input mutated")
	}
	cfg := adapter.DefaultConfig()
	cfg.Space = "unit"
	calls := []adapter.GraphCall{}
	for _, x := range append(append([]fixtureCall{}, in.Cases...), in.Supplemental...) {
		m := mappedCall("run01", x)
		calls = append(calls, adapter.GraphCall{Namespace: m.Namespace, Graphs: m.Graphs})
	}
	manifest, err := adapter.BuildManifest(cfg, "run01", calls)
	if err != nil {
		t.Fatal(err)
	}
	// Nine entities, eight relation records, twelve source records; 16 business
	// link edges + 12 provenance edges. Independent counts from fixed A input.
	if len(manifest.Vertices) != 29 || len(manifest.Edges) != 28 {
		t.Fatalf("manifest counts %d/%d", len(manifest.Vertices), len(manifest.Edges))
	}
	if len(mappedState("run01", out.Baseline).Entities) != 9 {
		t.Fatal("golden mapping")
	}
}
