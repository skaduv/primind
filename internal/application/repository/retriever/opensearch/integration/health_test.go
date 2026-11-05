package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Independently enumerate the observer's required schema rather than deriving
// test coverage from the implementation's list of required fields.
func validHealthFields(c Config) map[string]any {
	return map[string]any{
		"run_id": c.RunID, "budget_ref": c.Performance.BudgetRef, "at": time.Now().UTC().Format(time.RFC3339Nano),
		"stop": false, "host_cpu_percent": 0, "host_available_bytes": 1 << 30, "disk_free_bytes": 1 << 30,
		"os_rss_bytes": 1 << 20, "os_heap_bytes": 1 << 19, "os_write_queue": 0, "os_rejected_delta": 0,
		"existing_service_p95_milliseconds": 0,
	}
}

func writeHealthFields(t *testing.T, c Config, fields map[string]any) {
	t.Helper()
	b, e := json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(c.Performance.HealthFile, b, 0o600); e != nil {
		t.Fatal(e)
	}
}

func TestHealthRequiredObservationsFailClosedBeforeNetwork(t *testing.T) {
	keys := []string{
		"run_id",
		"budget_ref",
		"at",
		"stop",
		"host_cpu_percent",
		"host_available_bytes",
		"disk_free_bytes",
		"os_rss_bytes",
		"os_heap_bytes",
		"os_write_queue",
		"os_rejected_delta",
		"existing_service_p95_milliseconds",
	}
	for _, key := range keys {
		for _, mode := range []string{"omitted", "null", "wrong-type"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				c := performanceConfig(t)
				m := newMock(t, c.Performance.Rounds[0].Stores, false)
				c.Endpoint = m.server.URL
				fields := validHealthFields(c)
				switch mode {
				case "omitted":
					delete(fields, key)
				case "null":
					fields[key] = nil
				case "wrong-type":
					fields[key] = []int{1}
				}
				writeHealthFields(t, c, fields)
				if _, e := health(c); e == nil {
					t.Fatal("unknown/invalid observation accepted")
				}
				result, e := runPerformance(context.Background(), c)
				m.mu.Lock()
				defer m.mu.Unlock()
				if e == nil || result.Status != "FAIL" || len(result.Rounds) != 0 || m.Requests != 0 || m.Writes != 0 {
					t.Fatalf(
						"network before valid health: result=%+v err=%v requests=%d writes=%d",
						result,
						e,
						m.Requests,
						m.Writes,
					)
				}
			})
		}
	}
}

func TestHealthValidZerosAndRejectedRanges(t *testing.T) {
	cases := []struct {
		name, key string
		value     any
	}{
		{"measured-zero-and-false", "", nil},
		{"stale", "at", time.Now().Add(-time.Hour).Format(time.RFC3339Nano)},
		{"future", "at", time.Now().Add(time.Hour).Format(time.RFC3339Nano)},
		{"other-run", "run_id", "other-run"},
		{"other-budget", "budget_ref", "other-budget"},
		{"observer-stop", "stop", true},
		{"cpu-negative", "host_cpu_percent", -1},
		{"cpu-over100", "host_cpu_percent", 101},
		{"memory-zero", "host_available_bytes", 0},
		{"disk-zero", "disk_free_bytes", 0},
		{"rss-zero", "os_rss_bytes", 0},
		{"heap-zero", "os_heap_bytes", 0},
		{"queue-negative", "os_write_queue", -1},
		{"rejections-negative", "os_rejected_delta", -1},
		{"rejections-positive", "os_rejected_delta", 1},
		{"p95-negative", "existing_service_p95_milliseconds", -1},
		{"bytes-negative", "host_available_bytes", -1},
		{"integer-fraction", "os_write_queue", 0.5},
		{"extra-field", "unrecognized", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := performanceConfig(t)
			fields := validHealthFields(c)
			if tc.key != "" {
				fields[tc.key] = tc.value
			}
			writeHealthFields(t, c, fields)
			h, e := health(c)
			if (tc.key == "") != (e == nil) {
				t.Fatalf("sample=%+v error=%v", h, e)
			}
			if tc.key == "" &&
				(h.Stop || h.HostCPUPercent != 0 || h.OSWriteQueue != 0 || h.OSRejectedDelta != 0 ||
					h.ExistingServiceP95Milliseconds != 0) {
				t.Fatal("real zero/false not retained")
			}
		})
	}
}

func TestHealthInvalidDuringLoadStopsDispatchAndDrains(t *testing.T) {
	for _, mode := range []string{"missing-stop", "null-cpu", "observer-stop"} {
		t.Run(mode, func(t *testing.T) {
			c := performanceConfig(t)
			round := c.Performance.Rounds[3]
			m := newMock(t, round.Stores, false)
			var bulkCalls atomic.Int32
			firstArrived, secondArrived := make(chan struct{}), make(chan struct{})
			releaseFirst, peerCanceled := make(chan struct{}), make(chan struct{})
			// A parent deadline is a finite test cleanup boundary, not the stop signal.
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_bulk" {
					body, e := io.ReadAll(r.Body)
					if e != nil {
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					switch bulkCalls.Add(1) {
					case 1:
						close(firstArrived)
						select {
						case <-releaseFirst:
						case <-ctx.Done():
							return
						}
					case 2:
						close(secondArrived)
						select {
						case <-r.Context().Done():
							close(peerCanceled)
						case <-ctx.Done():
						}
						return
					default:
						http.Error(w, "unexpected further bulk", 500)
						return
					}
				}
				m.serve(w, r)
			}))
			defer front.Close()
			c.Endpoint = front.URL
			type outcome struct {
				result RoundResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() { r, e := runRound(ctx, c, round); done <- outcome{r, e} }()
			for _, arrived := range []chan struct{}{firstArrived, secondArrived} {
				select {
				case <-arrived:
				case <-ctx.Done():
					t.Fatal("two in-flight formal writes not reached")
				}
			}
			fields := validHealthFields(c)
			switch mode {
			case "missing-stop":
				delete(fields, "stop")
			case "null-cpu":
				fields["host_cpu_percent"] = nil
			case "observer-stop":
				fields["stop"] = true
			}
			writeHealthFields(t, c, fields)
			stoppedAt := time.Now()
			close(releaseFirst)
			var out outcome
			select {
			case out = <-done:
			case <-ctx.Done():
				t.Fatal("did not stop and drain before test deadline")
			}
			if out.err == nil || !strings.Contains(out.err.Error(), "health") || out.result.HealthFailure == "" ||
				out.result.Status != "FAIL" ||
				bulkCalls.Load() != 2 ||
				time.Since(stoppedAt) > time.Second {
				t.Fatalf(
					"invalid health did not stop dispatch: status=%s health_failure=%s error=%v bulks=%d",
					out.result.Status,
					out.result.HealthFailure,
					out.err,
					bulkCalls.Load(),
				)
			}
			if b := out.result.Budget; b.InFlight != 0 || b.Writing != 0 || b.Queued != 0 {
				t.Fatalf("budget not drained: %+v", b)
			}
			select {
			case <-peerCanceled:
			case <-time.After(time.Second):
				t.Fatal("in-flight peer request not canceled")
			}
		})
	}
}
