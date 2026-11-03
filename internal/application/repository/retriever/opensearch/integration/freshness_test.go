package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReplayFreshnessBarrier(t *testing.T) {
	cases := []struct {
		mode string
		at   int
		pass bool
	}{
		{"stale-before-write", 0, true}, {"eventual-correct", 0, true},
		{"refreshed-corrupt", 1, false}, {"refreshed-corrupt", 4, false}, {"never-refresh", 1, false},
		{"search-missing-version", 1, false}, {"search-null-seq", 1, false}, {"get-wrong-id", 1, false},
		{"get-missing-term", 1, false}, {"keyword-stale-copy", 1, false},
	}
	for at := 1; at <= 4; at++ {
		cases = append(cases, struct {
			mode string
			at   int
			pass bool
		}{"no-advance", at, false}, struct {
			mode string
			at   int
			pass bool
		}{"wrong-id-write", at, false})
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s-step%d", tc.mode, tc.at), func(t *testing.T) {
			c := config()
			s := *c.Replay
			m := newMock(t, []Scope{s}, true)
			originalDoc := original(s)
			idx := m.indexes[s.Alias]
			idx.docs[s.ChunkID] = originalDoc
			if tc.mode == "stale-before-write" {
				idx.revisions = map[string]Revision{s.ChunkID: {2, 1, 1}}
			}
			var viewMu sync.Mutex
			polls := map[int]int{}
			realGets := 0
			newGETSupported := true
			unforced := true
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				viewMu.Lock()
				defer viewMu.Unlock()
				if r.URL.Query().Get("refresh") == "true" || strings.HasSuffix(r.URL.Path, "/_refresh") {
					unforced = false
					http.Error(w, "forced refresh forbidden", 400)
					return
				}
				m.mu.Lock()
				writes := m.Writes
				v := idx.currentRevision(s.ChunkID)
				d := idx.docs[s.ChunkID]
				m.mu.Unlock()
				if r.URL.Path == "/"+s.Alias+"/_doc/"+s.ChunkID && r.Method == "GET" {
					realGets++
					if r.URL.Query().Get("realtime") != "true" || r.URL.Query().Get("refresh") != "false" {
						newGETSupported = false
					}
					body := map[string]any{"_id": s.ChunkID, "_index": s.Physical, "found": true, "_version": v.Version, "_seq_no": v.SeqNo, "_primary_term": v.PrimaryTerm, "_source": d}
					if writes == tc.at && tc.at > 0 {
						switch tc.mode {
						case "get-wrong-id":
							body["_id"] = "other-chunk"
						case "get-missing-term":
							delete(body, "_primary_term")
						}
					}
					respond(w, body)
					return
				}
				if r.URL.Path == "/"+s.keywordIndex()+"/_doc/"+s.ChunkID && r.Method == "GET" && tc.mode == "keyword-stale-copy" && writes == tc.at {
					// Its search view remains empty. Realtime existence must still fail.
					respond(w, map[string]any{"found": true, "_id": s.ChunkID})
					return
				}
				if r.URL.Path == "/"+s.Alias+"/_search" {
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil {
						t.Error("invalid search request")
						http.Error(w, "bad", 400)
						return
					}
					if body["version"] != true || body["seq_no_primary_term"] != true {
						t.Error("search must request revision metadata")
					}
					_, after := body["search_after"]
					if !after {
						polls[writes]++
					}
					stale := tc.mode == "stale-before-write" && writes == 0 && polls[0] < 3
					stale = stale || (tc.mode == "eventual-correct" && writes > 0 && polls[writes] < 3)
					stale = stale || ((tc.mode == "refreshed-corrupt" || tc.mode == "never-refresh") && writes == tc.at && (polls[writes] < 3 || tc.mode == "never-refresh"))
					if stale {
						v.Version--
						v.SeqNo--
						if writes == 0 {
							d.TagID = "different-old-search-view"
						}
					}
					if tc.mode == "refreshed-corrupt" && writes == tc.at && !stale {
						d.TagID = "corruption-visible-after-refresh"
						m.mu.Lock()
						idx.docs[s.ChunkID] = d
						m.mu.Unlock()
					}
					hits := []any{}
					if !after {
						hit := map[string]any{"_id": s.ChunkID, "_index": s.Physical, "_source": d, "sort": []string{s.ChunkID}, "_version": v.Version, "_seq_no": v.SeqNo, "_primary_term": v.PrimaryTerm}
						if writes == tc.at && tc.at > 0 {
							switch tc.mode {
							case "search-missing-version":
								delete(hit, "_version")
							case "search-null-seq":
								hit["_seq_no"] = nil
							}
						}
						hits = append(hits, hit)
					}
					respond(w, map[string]any{"timed_out": false, "_shards": map[string]int{"total": 1, "successful": 1, "failed": 0}, "hits": map[string]any{"total": map[string]any{"value": 1, "relation": "eq"}, "hits": hits}})
					return
				}
				isWrite := r.URL.Path == "/_bulk" || (r.Method == "PUT" && r.URL.Path == "/"+s.Alias+"/_doc/"+s.ChunkID)
				m.serve(w, r)
				if isWrite {
					m.mu.Lock()
					defer m.mu.Unlock()
					if m.Writes == tc.at && (tc.mode == "no-advance" || tc.mode == "wrong-id-write") {
						idx.revisions[s.ChunkID] = v
						idx.docs[s.ChunkID] = d
						if tc.mode == "wrong-id-write" {
							wrong := d
							wrong.ChunkID = "wrong-chunk-id"
							idx.persist(wrong)
						}
					}
				}
			}))
			defer front.Close()
			c.Endpoint = front.URL
			started := time.Now()
			result, err := runReplay(context.Background(), c)
			viewMu.Lock()
			defer viewMu.Unlock()
			m.mu.Lock()
			defer m.mu.Unlock()
			if !newGETSupported || realGets < 3 || !unforced {
				t.Fatal("fixture did not support the real revision protocol or forced refresh used")
			}
			if tc.pass {
				if err != nil || result.Status != "PASS" || m.Writes != 4 || len(result.Steps) != 5 {
					t.Fatalf("result=%+v err=%v writes=%d", result, err, m.Writes)
				}
				for i, step := range result.Steps {
					if step.AfterRevision == nil || step.SearchRevision == nil || *step.AfterRevision != *step.SearchRevision {
						t.Fatalf("missing fresh search receipt at %d", i)
					}
					if i > 0 && (step.BeforeRevision == nil || !step.AfterRevision.advanced(*step.BeforeRevision)) {
						t.Fatal("step did not advance")
					}
				}
				if tc.mode == "stale-before-write" && polls[0] < 3 {
					t.Fatal("wrote before baseline became searchable")
				}
				if tc.mode == "eventual-correct" {
					for n := 1; n <= 4; n++ {
						if polls[n] < 3 {
							t.Fatalf("step %d accepted pre-write search", n)
						}
					}
				}
			} else {
				if err == nil || result.Status == "PASS" || m.Writes != tc.at || len(result.Steps) != tc.at+1 {
					t.Fatalf("must stop on step %d, actual writes=%d result=%+v err=%v", tc.at, m.Writes, result, err)
				}
				if strings.Contains(err.Error(), "HTTP status") {
					t.Fatalf("unsupported fixture route masquerades as rejection: %v", err)
				}
				if tc.mode == "never-refresh" && (polls[tc.at] < 2 || time.Since(started) < time.Second || time.Since(started) > 3*time.Second) {
					t.Fatal("not a bounded stale-version timeout")
				}
				if tc.mode == "refreshed-corrupt" && polls[tc.at] < 3 {
					t.Fatal("did not reach fresh corrupt search generation")
				}
			}
			if result.Budget.InFlight != 0 || result.Budget.Writing != 0 || result.Budget.Queued != 0 {
				t.Fatal("replay did not drain")
			}
		})
	}
}

func TestRevisionValidationAndOrdering(t *testing.T) {
	one, zero, two, minus := int64(1), int64(0), int64(2), int64(-1)
	if _, e := revision(&one, &zero, &one); e != nil {
		t.Fatal("valid seq zero rejected")
	}
	for _, args := range [][3]*int64{{nil, &zero, &one}, {&one, nil, &one}, {&one, &zero, nil}, {&zero, &zero, &one}, {&one, &minus, &one}, {&one, &zero, &zero}} {
		if _, e := revision(args[0], args[1], args[2]); e == nil {
			t.Fatal("missing/invalid revision accepted")
		}
	}
	if !(Revision{two, two, one}).advanced(Revision{one, zero, one}) {
		t.Fatal("bounded SDK retries may advance more than one")
	}
	for _, got := range []Revision{{2, 0, 1}, {1, 1, 1}, {3, 2, 1}, {2, 1, 2}} {
		if _, e := got.reached(Revision{2, 1, 1}); e == nil {
			t.Fatalf("inconsistent/future revision accepted: %+v", got)
		}
	}
}
