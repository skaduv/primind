package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The search view deliberately remains at the pre-write refresh generation.
// The acknowledged durable view is corrupted by a simulated regression.
func TestCoordinatorReplayRejectsStalePreWriteSearchEvidence(t *testing.T) {
	s := scope()
	old := original(s)
	m := newMock(t, []Scope{s}, true)
	m.indexes[s.Alias].docs[old.ChunkID] = old
	var realtimeReads atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/"+s.Alias+"/_doc/"+old.ChunkID {
			realtimeReads.Add(1)
		}
		if r.URL.Path == "/"+s.Alias+"/_search" {
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			hits := []any{}
			if _, after := body["search_after"]; !after {
				hits = append(hits, map[string]any{"_id": old.ChunkID, "_index": s.Physical, "_source": old, "sort": []string{old.ChunkID}, "_version": 1, "_seq_no": 0, "_primary_term": 1})
			}
			respond(w, map[string]any{"timed_out": false, "_shards": map[string]int{"total": 1, "successful": 1, "failed": 0}, "hits": map[string]any{"total": map[string]any{"value": 1, "relation": "eq"}, "hits": hits}})
			return
		}
		m.serve(w, r)
		if r.URL.Path == "/_bulk" || (r.Method == "PUT" && r.URL.Path == "/"+s.Alias+"/_doc/"+old.ChunkID) {
			m.mu.Lock()
			bad := m.indexes[s.Alias].docs[old.ChunkID]
			bad.TagID = "post-write-corruption-hidden-by-stale-search"
			m.indexes[s.Alias].docs[old.ChunkID] = bad
			m.mu.Unlock()
		}
	}))
	defer front.Close()
	c := config()
	c.Endpoint = front.URL
	got, err := runReplay(context.Background(), c)
	m.mu.Lock()
	actual := m.indexes[s.Alias].docs[old.ChunkID]
	writes := m.Writes
	m.mu.Unlock()
	if actual.TagID == old.TagID || writes != 1 || realtimeReads.Load() < 3 || len(got.Steps) != 2 || got.Steps[1].AfterRevision == nil || got.Steps[1].AfterRevision.Version != 2 {
		t.Fatalf("fixture failed to retain acknowledged corruption: writes=%d tag=%s", writes, actual.TagID)
	}
	if err == nil || got.Status == "PASS" {
		t.Fatalf("false PASS from stale pre-write search view: status=%s steps=%d writes=%d actual_tag=%s", got.Status, len(got.Steps), writes, actual.TagID)
	}
	if !strings.Contains(err.Error(), "post-write realtime fields/vector mismatch") {
		t.Fatalf("must reject corruption, not an unsupported new route: %v", err)
	}
}

func TestCoordinatorHealthRejectsAbsentStopObservations(t *testing.T) {
	c := config()
	path := filepath.Join(t.TempDir(), "missing-health-fields.json")
	c.Performance = &Performance{HealthFile: path, HealthMaxAgeSeconds: 10, BudgetRef: "local-only", MaxHeapBytes: 1 << 40}
	// Counter/latency/stop fields are absent rather than observed zero/false.
	b, _ := json.Marshal(map[string]any{"run_id": c.RunID, "budget_ref": "local-only", "at": time.Now(), "host_available_bytes": 1 << 30, "disk_free_bytes": 1 << 30, "os_rss_bytes": 1024, "os_heap_bytes": 1024})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := health(c); err == nil {
		t.Fatal("incomplete health feed accepted as observed stop=false, CPU/queue/rejections/p95=0")
	}
}
