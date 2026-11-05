package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func performanceConfig(t *testing.T) Config {
	t.Helper()
	c := config()
	c.Mode = "performance"
	c.Replay = nil
	c.MaxRecords = 3000
	c.PageSize = 100
	c.Performance = &Performance{
		AllowIndexCreationAndOrphanCleanup: true, ProfileRef: "local-mock-profile", BudgetRef: "local-mock-budget",
		Seed: "fixed-seed", MaxDocuments: 2000, MaxConcurrency: 2, MaxHeapBytes: 1 << 30,
		HealthFile: filepath.Join(t.TempDir(), "health.json"), HealthMaxAgeSeconds: 30,
		Application: &Application{
			Endpoint:          "http://127.0.0.1:1",
			TokenEnv:          "OS02_SECRET_APP",
			KnowledgeBaseID:   "real-app-kb",
			KnowledgeAID:      "real-app-doc",
			EvidenceAChunkIDs: []string{"real-app-chunk"},
			MatchCount:        5,
		},
	}
	for variant := 0; variant < 3; variant++ {
		for repetition := 0; repetition < 3; repetition++ {
			id := string(rune('a'+variant)) + string(rune('a'+repetition))
			r := Round{
				ID:                        id,
				Variant:                   string(rune('a' + variant)),
				BatchSize:                 40,
				Concurrency:               1,
				DocumentsPerStore:         82,
				QueryIntervalMilliseconds: 10,
			}
			if variant > 0 {
				r.Concurrency = 2
			}
			if variant == 2 {
				r.Mixed = true
				r.MinimumSeconds = 1
				r.MinimumQueries = 3
			}
			for store := 0; store < 2; store++ {
				s := scope()
				s.StoreID = "abcdef0123456789"
				if store == 1 {
					s.StoreID = "012345abcdef6789"
				}
				prefix := "os02perf_local_" + id + string(rune('a'+store))
				s.Index.IndexName = prefix
				s.Alias = prefix + "_" + s.StoreID[:12] + "_3"
				s.Physical = s.Alias + "_v1"
				s.Namespace = prefix
				s.KnowledgeBaseID = prefix + "_kb"
				s.KnowledgeID = prefix + "_doc"
				s.ChunkID = ""
				r.Stores = append(r.Stores, s)
			}
			c.Performance.Rounds = append(c.Performance.Rounds, r)
		}
	}
	writeHealth(t, c, false)
	return c
}

func writeHealth(t *testing.T, c Config, stop bool) {
	t.Helper()
	h := HealthSample{
		RunID:              c.RunID,
		BudgetRef:          c.Performance.BudgetRef,
		At:                 time.Now(),
		Stop:               stop,
		HostCPUPercent:     5,
		HostAvailableBytes: 1 << 30,
		DiskFreeBytes:      1 << 30,
		OSRSSBytes:         1 << 20,
		OSHeapBytes:        1 << 19,
	}
	b, _ := json.Marshal(h)
	if e := os.WriteFile(c.Performance.HealthFile, b, 0o600); e != nil {
		t.Fatal(e)
	}
}

func TestPerformanceProfileRejectsMissingOrOverlappingBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"valid", func(*Config) {}},
		{"no-resource-budget", func(c *Config) { c.Performance.BudgetRef = "" }},
		{"inherited-health", func(c *Config) { c.Performance.HealthFile = "" }},
		{
			"same-index-next-round",
			func(c *Config) { c.Performance.Rounds[1].Stores[0] = c.Performance.Rounds[0].Stores[0] },
		},
		{"app-scope", func(c *Config) { c.Performance.Rounds[0].Stores[0].KnowledgeBaseID = "real-app-kb" }},
		{"one-store", func(c *Config) { c.Performance.Rounds[0].Stores = c.Performance.Rounds[0].Stores[:1] }},
		{"too-many-docs", func(c *Config) { c.Performance.MaxDocuments = 1 }},
		{"heap-envelope", func(c *Config) { c.Performance.MaxHeapBytes = 1024 }},
		{"no-creation-scope", func(c *Config) { c.Performance.AllowIndexCreationAndOrphanCleanup = false }},
		{"too-many-workers", func(c *Config) { c.Performance.Rounds[0].Concurrency = 3 }},
		{"insufficient-rounds", func(c *Config) { c.Performance.Rounds = c.Performance.Rounds[:8] }},
		{"no-app-hybrid", func(c *Config) { c.Performance.Application = nil }},
		{"no-minimum", func(c *Config) { c.Performance.Rounds[8].MinimumQueries = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := performanceConfig(t)
			tc.change(&c)
			e := c.validate(time.Now())
			if (tc.name == "valid") != (e == nil) {
				t.Fatalf("error=%v", e)
			}
		})
	}
}

func TestPerformanceFormalMultiStoreFullReconciliation(t *testing.T) {
	for _, fault := range []string{"", "bulk429"} {
		t.Run(fault, func(t *testing.T) {
			c := performanceConfig(t)
			r := c.Performance.Rounds[3]
			m := newMock(t, r.Stores, false)
			m.BulkDelay = 2 * time.Millisecond
			m.Fault = fault
			c.Endpoint = m.server.URL
			result, e := runRound(context.Background(), c, r)
			m.mu.Lock()
			defer m.mu.Unlock()
			if fault == "" {
				if e != nil || result.Status != "PASS" || result.CorrectUnique != 162 ||
					len(result.ActualManifests) != 2 ||
					result.BulkBytes <= 0 {
					t.Fatalf("result=%+v error=%v", result, e)
				}
				if result.Budget.Peak > c.TotalRequests || result.Budget.WritePeak > c.WriteRequests ||
					result.Budget.InFlight != 0 ||
					result.Budget.Queued != 0 {
					t.Fatal("shared budget violation")
				}
				if m.Bulks != 6 || m.Writes != 164 {
					t.Fatalf("wrong formal write counts %d/%d", m.Bulks, m.Writes)
				}
			} else {
				if e == nil || result.Status == "PASS" || m.Bulks > 2 {
					t.Fatalf("failure not stopped result=%+v error=%v bulks=%d", result, e, m.Bulks)
				}
			}
		})
	}
}

func TestPerformanceHealthGateZeroNetworkAndFiniteCancellation(t *testing.T) {
	c := performanceConfig(t)
	r := c.Performance.Rounds[0]
	m := newMock(t, r.Stores, false)
	c.Endpoint = m.server.URL
	writeHealth(t, c, true)
	if _, e := runPerformance(context.Background(), c); e == nil {
		t.Fatal("stop ignored")
	}
	if m.Requests != 0 {
		t.Fatal("network before health gate")
	}
	writeHealth(t, c, false)
	m.BulkDelay = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, e := runRound(ctx, c, r)
	if e == nil || time.Since(started) > time.Second || result.Budget.InFlight != 0 || result.Budget.Queued != 0 {
		t.Fatalf("cancel/drain failed error=%v budget=%+v", e, result.Budget)
	}
}

func TestPerformanceCannotReuseExistingIndex(t *testing.T) {
	c := performanceConfig(t)
	r := c.Performance.Rounds[0]
	m := newMock(t, r.Stores, true)
	c.Endpoint = m.server.URL
	if _, e := runRound(context.Background(), c, r); e == nil || m.Writes != 0 {
		t.Fatalf("existing index written error=%v", e)
	}
}

func TestPercentilesDoNotInventP99(t *testing.T) {
	samples := []Sample{
		{Kind: "query", Status: "PASS", Seconds: 1},
		{Kind: "query", Status: "PASS", Seconds: 2},
		{Kind: "query", Status: "FAIL", Seconds: 100},
	}
	q := summarize(samples)["query"]
	if q["n"] != 2 {
		t.Fatal(q)
	}
	if q["p95"] != 2 {
		t.Fatalf("nearest-rank p95 must retain the tail: %v", q)
	}
	if _, ok := q["p99"]; ok {
		t.Fatal("p99 on two samples")
	}
}

func TestMixedRoundUsesFormalRetrievalAndApplicationRRFEndpoint(t *testing.T) {
	c := performanceConfig(t)
	round := c.Performance.Rounds[6]
	m := newMock(t, round.Stores, false)
	c.Endpoint = m.server.URL
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/knowledge-bases/real-app-kb/hybrid-search" ||
			r.Header.Get("Authorization") != "Bearer synthetic-token" {
			http.Error(w, "wrong scope", http.StatusForbidden)
			return
		}
		respond(
			w,
			map[string]any{
				"success": true,
				"data": []any{
					map[string]any{
						"id":                "real-app-chunk",
						"knowledge_id":      "real-app-doc",
						"knowledge_base_id": "real-app-kb",
					},
				},
			},
		)
	}))
	defer app.Close()
	c.Performance.Application.Endpoint = app.URL
	t.Setenv("OS02_SECRET_APP", "synthetic-token")
	result, e := runRound(context.Background(), c, round)
	if e != nil || result.Status != "PASS" || result.WallSeconds < 1 || len(result.ApplicationEvidence) < 3 {
		t.Fatalf("error=%v result=%+v", e, result)
	}
	for _, kind := range []string{"index_keywords", "index_vector", "Q-KW", "Q-VEC", "Q-HYB"} {
		if result.Quantiles[kind]["n"] < 1 {
			t.Fatalf("no measured %s query", kind)
		}
	}
}

func TestPerformanceMissingHealthFeedDoesNotContactService(t *testing.T) {
	c := performanceConfig(t)
	m := newMock(t, c.Performance.Rounds[0].Stores, false)
	c.Endpoint = m.server.URL
	c.Performance.HealthFile = filepath.Join(t.TempDir(), "missing.json")
	if _, e := runPerformance(context.Background(), c); e == nil || m.Requests != 0 {
		t.Fatalf("error=%v requests=%d", e, m.Requests)
	}
}
