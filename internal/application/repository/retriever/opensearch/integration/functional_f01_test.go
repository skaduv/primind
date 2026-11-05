//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/opensearch"
	engines "github.com/primind/primind/internal/application/service/retriever"
	"github.com/primind/primind/internal/types"
	secutils "github.com/primind/primind/internal/utils"
)

const (
	f01Run    = "2026-10-29-functional-01"
	f01Output = "outputs/phase2-ai/OS-02/" + f01Run
)

// Fixed synthetic technical objects, never application DB IDs or model output.
func f01Plan(now time.Time) (Config, []Document) {
	s := Scope{
		StoreID: "f01a20261029store0001", Index: types.IndexConfig{IndexName: "primind_os02_f01_20261029", NumberOfShards: 1, NumberOfReplicas: 1, KNNEngine: "lucene", HNSWM: 16, HNSWEFConstruction: 100, HNSWEFSearch: 100}, Dimension: 4,
		Alias: "primind_os02_f01_20261029_f01a20261029_4", Physical: "primind_os02_f01_20261029_f01a20261029_4_v1",
		KnowledgeBaseID: "f01-synthetic-kb", KnowledgeID: "f01-synthetic-doc", ChunkID: "f01-chunk-a", RefreshInterval: "1s",
	}
	c := Config{
		Mode: "replay", RunID: f01Run, AuthorizationRef: "coordinator-F01-user-authorized-synthetic-only", Expires: now.Add(10 * time.Minute),
		Endpoint: "https://127.0.0.1:19200", ClusterUUID: "OGhRMiAtR3SAitp5LFtWKw", Version: "2.19.7", UsernameEnv: "OS02_SECRET_F01_USER", PasswordEnv: "OS02_SECRET_F01_PASSWORD",
		QuiescentScope: true, MaxSeconds: 300, VisibilitySeconds: 20, PollMilliseconds: 500, PageSize: 2, MaxRecords: 20, TotalRequests: 2, WriteRequests: 1, Waiters: 2, Replay: &s,
	}
	docs := []Document{
		{
			ChunkID:         s.ChunkID,
			SourceID:        "f01-source-a",
			SourceType:      0,
			KnowledgeID:     s.KnowledgeID,
			KnowledgeBaseID: s.KnowledgeBaseID,
			TagID:           "f01-tag-a",
			Content:         "Synthetic only F01ANCHOR seal replacement A",
			Enabled:         true,
			Recommended:     true,
			Embedding:       []float32{1, 0, 0, 0},
		},
		{
			ChunkID:         "f01-chunk-b",
			SourceID:        "f01-source-b",
			SourceType:      1,
			KnowledgeID:     s.KnowledgeID,
			KnowledgeBaseID: s.KnowledgeBaseID,
			TagID:           "f01-tag-b",
			Content:         "Synthetic only F01BETA bearing replacement B",
			Enabled:         true,
			Embedding:       []float32{0, 1, 0, 0},
		},
		{
			ChunkID:         "f01-chunk-disabled",
			SourceID:        "f01-source-disabled",
			SourceType:      2,
			KnowledgeID:     s.KnowledgeID,
			KnowledgeBaseID: s.KnowledgeBaseID,
			TagID:           "f01-tag-disabled",
			Content:         "Synthetic only F01ANCHOR disabled control",
			Enabled:         false,
			Recommended:     true,
			Embedding:       []float32{1, 0, 0, 0},
		},
		{
			ChunkID:         "f01-chunk-other-kb",
			SourceID:        "f01-source-other-kb",
			SourceType:      0,
			KnowledgeID:     "f01-other-doc",
			KnowledgeBaseID: "f01-other-kb",
			TagID:           "f01-tag-other",
			Content:         "Synthetic only F01ANCHOR other scope control",
			Enabled:         true,
			Embedding:       []float32{1, 0, 0, 0},
		},
		{
			ChunkID:         "f01-chunk-survivor",
			SourceID:        "f01-source-survivor",
			SourceType:      0,
			KnowledgeID:     s.KnowledgeID,
			KnowledgeBaseID: s.KnowledgeBaseID,
			TagID:           "f01-tag-survivor",
			Content:         "Synthetic only F01SURVIVOR retained before index cleanup",
			Enabled:         true,
			Embedding:       []float32{0, 0, 1, 0},
		},
		{
			ChunkID:         "f01-chunk-keyword",
			SourceID:        "f01-source-keyword",
			SourceType:      0,
			KnowledgeID:     s.KnowledgeID,
			KnowledgeBaseID: s.KnowledgeBaseID,
			TagID:           "f01-tag-keyword",
			Content:         "Synthetic only F01KEYWORD no vector",
			Enabled:         true,
		},
	}
	return c, docs
}

func TestF01FrozenScopeAndDefaultGate(t *testing.T) {
	c, docs := f01Plan(time.Now())
	if err := c.validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	if c.InsecureSkipVerify || c.Performance != nil || len(docs) > 20 || c.TotalRequests != 2 || c.MaxSeconds != 300 {
		t.Fatal("unsafe F01 plan")
	}
	seen := map[string]bool{}
	for _, d := range docs {
		if seen[d.ChunkID] || d.SourceID == d.ChunkID {
			t.Fatal("ambiguous synthetic identity")
		}
		seen[d.ChunkID] = true
		raw, _ := json.Marshal(d)
		if _, err := decodeDocument(raw, len(d.Embedding)); err != nil {
			t.Fatal(err)
		}
	}
	// Real entry returns before credentials, file I/O or network without its own exact opt-in.
	if f01Enabled("") || f01Enabled("performance") || !f01Enabled("F01_SYNTHETIC_ONLY") {
		t.Fatal("invalid functional gate")
	}
}

func f01Enabled(value string) bool { return value == "F01_SYNTHETIC_ONLY" }

type f01Journal struct {
	file *os.File
	mu   sync.Mutex
	t    *testing.T
}

func (j *f01Journal) add(action string, value any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := json.NewEncoder(j.file).Encode(map[string]any{"at_utc": time.Now().UTC(), "action": action, "evidence": value}); err != nil {
		j.t.Fatalf("journal write failed: %v", err)
	}
	if err := j.file.Sync(); err != nil {
		j.t.Fatalf("journal sync failed: %v", err)
	}
}

func TestRealFunctionalF01(t *testing.T) {
	f01Functional(t, "F01_SYNTHETIC_ONLY", "functional-events.jsonl")
}

func TestRealF01ReadOnlyReserved(t *testing.T) {
	f01Functional(t, "F01_READ_ONLY_RESERVED", "readonly-formal-events.jsonl")
}

func TestRealF01Resume(t *testing.T) {
	f01Functional(t, "F01_RESUME_VERIFIED", "resume-events.jsonl")
}

func f01Functional(t *testing.T, mode, evidenceName string) {
	if os.Getenv("OS02_F01_OPT_IN") != mode {
		t.Skip("NOT_RUN: F01 explicit opt-in absent")
	}
	initial := mode == "F01_SYNTHETIC_ONLY"
	if runtime.Version() != "go1.26.0" || runtime.GOMAXPROCS(0) != 2 {
		t.Fatal("F01 requires Go1.26.0 and GOMAXPROCS=2")
	}
	sha := os.Getenv("OS02_F01_COMMIT")
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(sha) {
		t.Fatal("fixed candidate commit required")
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Go test package cwd is fixed by the package; never use the chat default cwd.
	for i := 0; i < 6; i++ {
		root = filepath.Dir(root)
	}
	if filepath.Base(root) != "primind-opensearch" {
		t.Fatal("unexpected repository root")
	}
	path := filepath.Join(root, filepath.FromSlash(f01Output), evidenceName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal("evidence must be new; no automatic rerun")
	}
	defer f.Close()
	j := &f01Journal{file: f, t: t}
	state := "FAIL_OR_UNKNOWN_PRESERVED"
	defer func() { j.add("final", map[string]any{"status": state, "commit": sha, "no_failure_cleanup": true}) }()
	fail := func(stage string, e error) {
		t.Helper()
		if e != nil {
			j.add(
				"failure",
				map[string]any{
					"stage":          stage,
					"error":          e.Error(),
					"further_writes": "STOPPED",
					"resources":      "preserved; reconcile before any retry",
				},
			)
			t.Fatalf("%s: %v", stage, e)
		}
	}
	c, docs := f01Plan(time.Now())
	fail("validate", c.validate(time.Now()))
	var recovery struct {
		Objects map[string]struct {
			UUID         string `json:"uuid"`
			CreationDate string `json:"creation_date"`
		} `json:"objects"`
	}
	if !initial {
		raw, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(f01Output), "recovery-manifest.json"))
		fail("recovery manifest", e)
		fail("recovery manifest JSON", json.Unmarshal(raw, &recovery))
		if len(recovery.Objects) != 2 {
			fail("recovery object scope", errors.New("exactly two owned indices required"))
		}
		if mode == "F01_RESUME_VERIFIED" {
			prior, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(f01Output), "readonly-formal-events.jsonl"))
			fail("prior formal readonly evidence", e)
			var last struct {
				Action   string `json:"action"`
				Evidence struct {
					Status string `json:"status"`
					Commit string `json:"commit"`
				} `json:"evidence"`
			}
			lines := bytes.Split(bytes.TrimSpace(prior), []byte("\n"))
			fail("prior final receipt", json.Unmarshal(lines[len(lines)-1], &last))
			if last.Action != "final" || last.Evidence.Status != "PASS_READONLY_FULL_MANIFEST" ||
				last.Evidence.Commit != sha {
				fail(
					"prior fixed readonly run",
					errors.New("same fixed candidate must first verify owned objects without writing"),
				)
			}
		}
	}
	j.add(
		"frozen_plan",
		map[string]any{
			"commit":                 sha,
			"config":                 c,
			"synthetic":              true,
			"documents":              manifest(f01Map(docs)),
			"unique_chunks":          len(docs),
			"maximum_inflight":       2,
			"normal_refresh":         true,
			"application_and_models": "NOT_RUN",
		},
	)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "OS02_REAL_OPT_IN", "OS02_REAL_CONFIG"} {
		t.Setenv(name, "")
	}
	secutils.SetSSRFWhitelistFromRaw("127.0.0.1")
	defer secutils.SetSSRFWhitelistFromRaw("")
	ca, err := os.ReadFile("C:/Users/11756/AppData/Local/Primind/certs/opensearch-dev/root-ca.pem")
	fail("read public CA", err)
	block, _ := pem.Decode(ca)
	if block == nil || block.Type != "CERTIFICATE" {
		fail("CA", errors.New("invalid public CA PEM"))
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	fail("CA parse", err)
	caHash := sha256.Sum256(cert.Raw)
	j.add(
		"ca_fingerprint",
		map[string]any{
			"sha256_der":   hex.EncodeToString(caHash[:]),
			"not_before":   cert.NotBefore,
			"not_after":    cert.NotAfter,
			"trust_action": "read only; use existing Windows system roots",
		},
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	stats := &connectionStats{}
	ctx = stats.context(ctx)
	ctx = httptrace.WithClientTrace(
		ctx,
		&httptrace.ClientTrace{TLSHandshakeDone: func(cs tls.ConnectionState, e error) {
			value := map[string]any{
				"verified_chains": len(cs.VerifiedChains),
				"tls_version":     cs.Version,
				"success":         e == nil,
			}
			if e != nil {
				value["error"] = e.Error()
			}
			if len(cs.PeerCertificates) > 0 {
				h := sha256.Sum256(cs.PeerCertificates[0].Raw)
				value["leaf_sha256_der"] = hex.EncodeToString(h[:])
			}
			j.add("formal_go_tls", value)
		}},
	)
	budget, err := adapter.NewRequestBudget(2, 1, 2)
	fail("budget", err)
	defer func() { j.add("runtime", map[string]any{"budget": budget.Snapshot(), "connections": stats.snapshot()}) }()
	cc, err := clientConfig(c)
	fail("credential references", err)
	ro := adapter.DefaultRuntimeOptions()
	ro.MaxConnsPerHost = 2
	ro.MaxIdleConns = 2
	ro.MaxIdleConnsPerHost = 2
	client, err := adapter.NewOpenSearchClient(cc, adapter.WithRequestBudget(budget), adapter.WithRuntimeOptions(ro))
	fail("formal client", err)
	defer adapter.CloseIdleConnections(client)
	r := reader{client: client, config: c}
	var info struct {
		ClusterUUID string `json:"cluster_uuid"`
		ClusterName string `json:"cluster_name"`
		Version     struct {
			Number       string `json:"number"`
			Distribution string `json:"distribution"`
		} `json:"version"`
	}
	_, err = r.json(ctx, "GET", "/", nil, &info)
	fail("identity read", err)
	j.add("cluster_identity", info)
	if info.ClusterUUID != c.ClusterUUID || info.ClusterName != "primind-os-dev" || info.Version.Number != c.Version ||
		info.Version.Distribution != "opensearch" {
		fail("identity", errors.New("cluster identity mismatch"))
	}
	var nodes struct {
		Summary struct {
			Total      int `json:"total"`
			Successful int `json:"successful"`
			Failed     int `json:"failed"`
		} `json:"_nodes"`
		Nodes map[string]struct {
			Version string `json:"version"`
			Plugins []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"plugins"`
		} `json:"nodes"`
	}
	_, err = r.json(
		ctx,
		"GET",
		"/_nodes/plugins?filter_path=_nodes,nodes.*.version,nodes.*.plugins.name,nodes.*.plugins.version",
		nil,
		&nodes,
	)
	fail("nodes/plugins", err)
	j.add("nodes_plugins", nodes)
	if nodes.Summary.Total != 2 || nodes.Summary.Successful != 2 || nodes.Summary.Failed != 0 || len(nodes.Nodes) != 2 {
		fail("nodes", errors.New("expected two complete node observations"))
	}
	for _, n := range nodes.Nodes {
		found := false
		for _, p := range n.Plugins {
			found = found || p.Name == "opensearch-knn"
		}
		if n.Version != c.Version || !found {
			fail("plugins", errors.New("node version/kNN differs"))
		}
	}
	s := *c.Replay
	objects := []string{s.Alias, s.Physical, s.keywordIndex()}
	if initial {
		for _, name := range objects {
			status, e := r.json(ctx, "HEAD", "/"+name, nil, nil)
			j.add("before_absent", map[string]any{"object": name, "http_status": status})
			if status != 404 {
				if e == nil {
					e = errors.New("object already exists")
				}
				fail("exclusive empty scope", e)
			}
		}
	}
	repo, err := adapter.NewRepository(
		ctx,
		client,
		s.StoreID,
		&s.Index,
		adapter.WithWriteObserver(func(o adapter.WriteObservation) { j.add("bulk_observation", o) }),
	)
	fail("formal repository", err)
	sess := &session{reader: r, repo: repo}
	if initial {
		infos, params := parameters(docs[:1])
		err = repo.Save(ctx, infos[0], params)
		fail("Save anchor (unknown on error)", err)
		j.add("Save", map[string]any{"id": docs[0].ChunkID, "status": "acknowledged"})
		infos, params = parameters(docs[1:5])
		err = repo.BatchSave(ctx, infos, params)
		fail("BatchSave vectors (unknown on error)", err)
		infos, params = parameters(docs[5:])
		err = repo.BatchSave(ctx, infos, params)
		fail("BatchSave keyword (unknown on error)", err)
	}
	fail("vector settings/mapping", r.settings(ctx, s))
	fail("alias", r.alias(ctx, s))
	ks := s
	ks.Dimension = 0
	ks.Alias = s.keywordIndex()
	ks.Physical = ks.Alias
	fail("keyword settings/mapping", r.settings(ctx, ks))
	uuids := map[string]string{}
	for _, target := range []Scope{s, ks} {
		var settings map[string]struct {
			Settings map[string]string `json:"settings"`
		}
		_, err = r.json(ctx, "GET", "/"+target.Physical+"/_settings?flat_settings=true", nil, &settings)
		fail("created identity", err)
		u := settings[target.Physical].Settings["index.uuid"]
		if u == "" {
			fail("created UUID", errors.New("missing index UUID"))
		}
		if !initial {
			expected, ok := recovery.Objects[target.Physical]
			if !ok || expected.UUID != u ||
				expected.CreationDate != settings[target.Physical].Settings["index.creation_date"] {
				fail("preserved object identity", errors.New("UUID/creation date changed; no writes permitted"))
			}
		}
		uuids[target.Physical] = u
		j.add("created_index", map[string]any{"physical": target.Physical, "uuid": u, "settings": settings})
		var mapping json.RawMessage
		_, err = r.json(ctx, "GET", "/"+target.Physical+"/_mapping", nil, &mapping)
		fail("mapping evidence", err)
		j.add("mapping", mapping)
	}
	// Per-item evidence uses exact realtime GET plus independent full-field hashes.
	for _, d := range docs {
		target := s
		target.ChunkID = d.ChunkID
		target.KnowledgeID = d.KnowledgeID
		target.KnowledgeBaseID = d.KnowledgeBaseID
		if len(d.Embedding) == 0 {
			target = ks
			target.ChunkID = d.ChunkID
		}
		got, rev, e := r.realtime(ctx, target)
		fail("per-item GET", e)
		if got.hash() != d.hash() {
			fail("per-item full fields", errors.New("mismatch"))
		}
		j.add(
			"item_verified",
			map[string]any{
				"id":          d.ChunkID,
				"physical":    target.Physical,
				"revision":    rev,
				"source_hash": got.hash(),
				"vector_hash": got.vectorHash(),
				"dimension":   len(got.Embedding),
			},
		)
	}
	fail("normal-refresh vector visibility", f01Visible(ctx, r, s, f01Map(docs[:5])))
	fail("normal-refresh keyword visibility", f01Visible(ctx, r, ks, f01Map(docs[5:])))
	j.add(
		"full_reconciliation",
		map[string]any{
			"vector":         manifest(f01Map(docs[:5])),
			"keyword":        manifest(f01Map(docs[5:])),
			"status":         "PASS",
			"forced_refresh": false,
		},
	)
	if mode == "F01_READ_ONLY_RESERVED" {
		state = "PASS_READONLY_FULL_MANIFEST"
		return
	}
	// Same exact synthetic anchor; reuse the existing freshness/barrier implementation.
	d, previous, err := sess.realtime(ctx, s)
	fail("replay baseline", err)
	before, observed, err := sess.visibleRevision(ctx, s, d, previous)
	fail("replay baseline visibility", err)
	j.add(
		"synthetic_replay_baseline",
		map[string]any{"revision": previous, "search_revision": observed, "records": manifest(before)},
	)
	for _, action := range []string{"Save-1", "Save-2", "BatchSave-1", "BatchSave-2"} {
		step, next, e := replayStep(ctx, sess, s, d, previous, action)
		j.add("synthetic_replay_step", step)
		fail(action, e)
		previous = next
	}
	kw := types.RetrieveParams{
		RetrieverType:    types.KeywordsRetrieverType,
		Query:            "F01ANCHOR",
		KnowledgeBaseIDs: []string{s.KnowledgeBaseID},
		KnowledgeIDs:     []string{s.KnowledgeID},
		TopK:             10,
		Threshold:        0,
		AdditionalParams: map[string]any{"dim": 4},
	}
	vec := kw
	vec.RetrieverType = types.VectorRetrieverType
	vec.Embedding = []float32{1, 0, 0, 0}
	vec.Threshold = 0.9
	for _, p := range []types.RetrieveParams{kw, vec} {
		got, e := repo.Retrieve(ctx, p)
		fail("formal Retrieve", e)
		fail("exact expected query IDs", f01QueryCheck(got, p.RetrieverType, docs[0]))
		j.add("retrieve", got)
	}
	// Actual existing composite/hybrid engine fan-out; no HTTP app, model or RRF emulation.
	registry := engines.NewRetrieveEngineRegistry(nil, nil)
	fail(
		"in-memory registry",
		registry.Register(engines.NewKVHybridRetrieveEngine(repo, types.OpenSearchRetrieverEngineType)),
	)
	composite, err := engines.NewCompositeRetrieveEngine(
		registry,
		[]types.RetrieverEngineParams{
			{RetrieverEngineType: types.OpenSearchRetrieverEngineType, RetrieverType: types.KeywordsRetrieverType},
			{RetrieverEngineType: types.OpenSearchRetrieverEngineType, RetrieverType: types.VectorRetrieverType},
		},
	)
	fail("composite", err)
	got, err := composite.Retrieve(ctx, []types.RetrieveParams{kw, vec})
	fail("existing composite two-mode call", err)
	if len(got) != 2 {
		fail("composite bundles", errors.New("expected exactly keyword and vector bundles"))
	}
	seen := map[types.RetrieverType]bool{}
	for _, b := range got {
		if b == nil || seen[b.RetrieverType] {
			fail("composite modes", errors.New("nil or duplicate mode"))
		}
		seen[b.RetrieverType] = true
		fail("composite exact expected IDs", f01QueryCheck([]*types.RetrieveResult{b}, b.RetrieverType, docs[0]))
	}
	if !seen[types.KeywordsRetrieverType] || !seen[types.VectorRetrieverType] {
		fail("composite modes", errors.New("missing mode"))
	}
	j.add("existing_composite_results", map[string]any{"results": got, "application_rrf": "NOT_RUN"})
	// Limited deletion: delete only A, prove all four vector bystanders and keyword survive.
	err = repo.DeleteByChunkIDList(ctx, []string{s.ChunkID}, 4, "")
	fail("limited adapter deletion", err)
	fail("deletion remaining vector set", f01Visible(ctx, r, s, f01Map(docs[1:5])))
	fail("keyword bystander", f01Visible(ctx, r, ks, f01Map(docs[5:])))
	status, err := r.json(ctx, "GET", "/"+s.Alias+"/_doc/"+s.ChunkID+"?realtime=true", nil, nil)
	if status != 404 {
		if err == nil {
			err = errors.New("deleted ID still exists")
		}
		fail("deleted realtime absence", err)
	}
	j.add(
		"limited_delete",
		map[string]any{
			"deleted_id":             s.ChunkID,
			"realtime_status":        status,
			"vector_survivors":       manifest(f01Map(docs[1:5])),
			"keyword_survivors":      manifest(f01Map(docs[5:])),
			"status":                 "PASS",
			"adapter_delete_refresh": "existing adapter uses refresh=true",
		},
	)
	// Cleanup only pre-registered, initially absent physical indices whose UUIDs still match.
	for _, name := range []string{s.Physical, ks.Physical} {
		var settings map[string]struct {
			Settings map[string]string `json:"settings"`
		}
		_, err = r.json(ctx, "GET", "/"+name+"/_settings?flat_settings=true", nil, &settings)
		fail("cleanup identity", err)
		if len(settings) != 1 || settings[name].Settings["index.uuid"] != uuids[name] {
			fail("cleanup UUID", errors.New("ownership changed; preserve"))
		}
		var response struct {
			Acknowledged bool `json:"acknowledged"`
		}
		_, err = r.json(ctx, "DELETE", "/"+name, nil, &response)
		fail("exact index cleanup", err)
		if !response.Acknowledged {
			fail("cleanup acknowledgement", errors.New("unknown cleanup result"))
		}
		j.add("cleanup", map[string]any{"physical": name, "uuid": uuids[name], "acknowledged": true})
	}
	for _, name := range objects {
		status, err = r.json(ctx, "HEAD", "/"+name, nil, nil)
		j.add("after_absent", map[string]any{"object": name, "http_status": status})
		if status != 404 {
			if err == nil {
				err = errors.New("object remains")
			}
			fail("cleanup residual", err)
		}
	}
	b := budget.Snapshot()
	if b.InFlight != 0 || b.Writing != 0 || b.Queued != 0 || b.Peak > 2 || b.WritePeak > 1 {
		fail("runtime drain", errors.New("budget exceeded or not drained"))
	}
	state = "PASS_TECHNICAL_SYNTHETIC_ONLY"
}

func f01Map(docs []Document) map[string]Document {
	out := map[string]Document{}
	for _, d := range docs {
		out[d.ChunkID] = d
	}
	return out
}

func f01Visible(ctx context.Context, r reader, s Scope, want map[string]Document) error {
	return adapter.WaitForVisibility(
		ctx,
		20*time.Second,
		500*time.Millisecond,
		func(check context.Context) (bool, error) {
			got, err := r.scan(check, s, s.Physical, s.Dimension, false, false)
			if err != nil {
				return false, err
			}
			if len(got) != len(want) {
				return false, nil
			}
			return true, reconcile(want, got)
		},
	)
}

func f01QueryCheck(got []*types.RetrieveResult, mode types.RetrieverType, want Document) error {
	if len(got) != 1 || got[0] == nil || got[0].Error != nil || got[0].RetrieverType != mode ||
		got[0].RetrieverEngineType != types.OpenSearchRetrieverEngineType ||
		len(got[0].Results) != 1 {
		return errors.New("query expected exactly one complete anchor hit")
	}
	h := got[0].Results[0]
	if h == nil || h.ID != want.ChunkID || h.ChunkID != want.ChunkID || h.KnowledgeID != want.KnowledgeID ||
		h.KnowledgeBaseID != want.KnowledgeBaseID ||
		h.SourceID != want.SourceID ||
		int(h.SourceType) != want.SourceType ||
		h.TagID != want.TagID ||
		h.Content != want.Content ||
		!h.IsEnabled {
		return errors.New("query identity/content/filter mismatch")
	}
	return nil
}

// Stable diagnostic names for a reviewer; no secret or vector data is emitted.
func TestF01PlanEvidence(t *testing.T) {
	c, docs := f01Plan(time.Now())
	names := []string{c.Replay.Alias, c.Replay.Physical, c.Replay.keywordIndex()}
	sort.Strings(names)
	t.Logf("run=%s unique_chunks=%d objects=%v", f01Run, len(docs), names)
	t.Log(
		fmt.Sprintf(
			"model_calls=0 app_calls=0 max_seconds=%d max_inflight=%d replicas=%d",
			c.MaxSeconds,
			c.TotalRequests,
			c.Replay.Index.NumberOfReplicas,
		),
	)
}

// Actual redacted 2.19.7 responses contain arrays among unrelated defaults.
// Keep strict typing and equality for the required profile keys themselves.
func TestF01RealSettingsResponse(t *testing.T) {
	c, _ := f01Plan(time.Now())
	root, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		root = filepath.Dir(root)
	}
	dir := filepath.Join(root, filepath.FromSlash(f01Output))
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("F01 evidence directory %s is not in this checkout (outputs/ evidence is kept out of the repo)", f01Output)
	}
	for _, tc := range []struct {
		name                      string
		keyword, corrupt, missing bool
	}{
		{name: "vector_actual_response"},
		{name: "keyword_actual_response", keyword: true},
		{name: "required_string_cannot_be_array", corrupt: true},
		{name: "missing_required_setting", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := *c.Replay
			prefix := "vector"
			if tc.keyword {
				prefix = "keyword"
				s.Dimension = 0
				s.Physical = s.keywordIndex()
				s.Alias = s.Physical
			}
			settings, err := os.ReadFile(filepath.Join(dir, prefix+"-settings-raw.json"))
			if err != nil {
				t.Fatal(err)
			}
			mapping, err := os.ReadFile(filepath.Join(dir, prefix+"-mapping-raw.json"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.corrupt || tc.missing {
				var data map[string]map[string]map[string]json.RawMessage
				if err = json.Unmarshal(settings, &data); err != nil {
					t.Fatal(err)
				}
				if tc.corrupt {
					data[s.Physical]["settings"]["index.number_of_shards"] = json.RawMessage(`["1"]`)
				}
				if tc.missing {
					delete(data[s.Physical]["settings"], "index.number_of_shards")
					delete(data[s.Physical]["defaults"], "index.number_of_shards")
				}
				settings, err = json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					http.Error(w, "read only", 405)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/" + s.Physical + "/_settings":
					_, _ = w.Write(settings)
				case "/" + s.Physical + "/_mapping":
					_, _ = w.Write(mapping)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			secutils.SetSSRFWhitelistFromRaw("127.0.0.1")
			defer secutils.SetSSRFWhitelistFromRaw("")
			client, err := adapter.NewOpenSearchClient(&types.ConnectionConfig{Addr: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.CloseIdleConnections(client)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = (reader{client: client, config: c}).settings(ctx, s)
			if tc.corrupt || tc.missing {
				if err == nil {
					t.Fatal("invalid required setting accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
