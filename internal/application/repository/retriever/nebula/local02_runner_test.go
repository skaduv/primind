package nebula_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adapter "github.com/primind/primind/internal/application/repository/retriever/nebula"
	"github.com/primind/primind/internal/types"
)

// All observations below are synthetic offline controls, not service samples.
// The fake only tests the measurement runner's stop/drain logic. Formal adapter
// cancellation already has independent net.Pipe evidence in the existing tests.
type local02BlockedRepository struct {
	started chan struct{}
	active  atomic.Int32
	calls   atomic.Int32
}

func (r *local02BlockedRepository) AddGraph(ctx context.Context, _ types.NameSpace, _ []*types.GraphData) error {
	if r.calls.Add(1) == 1 {
		close(r.started)
	}
	r.active.Add(1)
	defer r.active.Add(-1)
	<-ctx.Done()
	return ctx.Err()
}

func (*local02BlockedRepository) SearchNode(context.Context, types.NameSpace, []string) (*types.GraphData, error) {
	return nil, errors.New("unexpected offline search")
}

func (r *local02BlockedRepository) Metrics() adapter.Metrics {
	return adapter.Metrics{Active: int(r.active.Load())}
}

func local02ResourceProfile() adapter.RunProfile {
	return adapter.RunProfile{
		Connections: 4, MaxCPUPercent: 80, MaxMemoryBytes: 1024, MinFreeDiskBytes: 100,
		MaxDiskIOBytesPerSecond: 100, MaxServiceLatencyMS: 10, MaxServiceErrorRate: .01,
	}
}

func local02ResourceSample() adapter.ResourceSample {
	return adapter.ResourceSample{
		At: time.Now(), CPUPercent: 1, MemoryBytes: 10, FreeDiskBytes: 1000, Connections: 1,
		Sessions: 1,
	}
}

func local02Output(t *testing.T) string {
	t.Helper()
	root := os.Getenv("PRIMIND_LOCAL02_ARTIFACTS")
	if root == "" {
		return t.TempDir()
	}
	if !filepath.IsAbs(root) {
		t.Fatal("LOCAL-02 evidence path must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := os.MkdirTemp(root, "local02-")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLocal02ResourceSnapshotParser(t *testing.T) {
	dir := local02Output(t)
	p := local02ResourceProfile()
	p.ResourceSamplePath = filepath.Join(dir, "synthetic-sample.json")
	base, err := json.Marshal(local02ResourceSample())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid-zero-error-rate", "missing-disk-io", "null-sessions", "stale"} {
		t.Run(mode, func(t *testing.T) {
			var data map[string]any
			if err := json.Unmarshal(base, &data); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing-disk-io":
				delete(data, "disk_write_bytes_per_second")
			case "null-sessions":
				data["sessions"] = nil
			case "stale":
				data["at"] = time.Now().Add(-time.Minute)
			}
			b, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ResourceSamplePath, b, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = sampleResource(p)
			if (err == nil) != (mode == "valid-zero-error-rate") {
				t.Fatalf("sample parser accepted missing/null/stale observation or rejected measured zero: %v", err)
			}
		})
	}
}

func TestLocal02RunnerStopsAndDrainsOnResourceLimit(t *testing.T) {
	for _, mode := range []string{"before-first-call", "in-flight"} {
		t.Run(mode, func(t *testing.T) {
			p := local02ResourceProfile()
			r := &local02BlockedRepository{started: make(chan struct{})}
			reader := func(p adapter.RunProfile) (adapter.ResourceSample, error) {
				s := local02ResourceSample()
				if mode == "before-first-call" {
					s.CPUPercent = 81
				} else {
					select {
					case <-r.started:
						s.CPUPercent = 81
					default:
					}
				}
				return s, p.CheckResource(s, time.Now())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls := []adapter.GraphCall{
				{Namespace: types.NameSpace{KnowledgeBase: "local", Knowledge: "one"}},
				{Namespace: types.NameSpace{KnowledgeBase: "local", Knowledge: "two"}},
			}
			got, err := replayWithSampler(ctx, p, r, calls, 1, "new", reader)
			if !errors.Is(err, adapter.ErrBudget) {
				t.Fatalf("stop threshold was not the returned cause: %v", err)
			}
			wantCalls := 0
			if mode == "in-flight" {
				wantCalls = 1
			}
			if got.Calls != wantCalls || got.Errors != wantCalls || int(r.calls.Load()) != wantCalls ||
				r.active.Load() != 0 || got.Metrics.Active != 0 {
				t.Fatalf("unexpected dispatch or incomplete drain: result=%+v calls=%d active=%d", got, r.calls.Load(),
					r.active.Load())
			}
			found := false
			for _, sample := range got.ResourceSamples {
				found = found || sample.CPUPercent == 81
			}
			if !found {
				t.Fatal("the stopping resource observation was lost from failure evidence")
			}
		})
	}
}

type local02ErrorRecorder struct {
	failures    int
	fatalErrors []error
}

func (r *local02ErrorRecorder) Error(...any) { r.failures++ }
func (r *local02ErrorRecorder) Fatal(args ...any) {
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			r.fatalErrors = append(r.fatalErrors, err)
		}
	}
}

func TestLocal02PerformanceEvidenceWriteFailure(t *testing.T) {
	t.Run("successful required checks", func(t *testing.T) {
		path := filepath.Join(local02Output(t), "performance-checks.json")
		reporter := &local02ErrorRecorder{}
		recordPerformanceEvidence(reporter, path, map[string]string{"P7": "NOT_RUN"})
		var got map[string]string
		if err := readJSONFile(path, &got); err != nil || got["P7"] != "NOT_RUN" || reporter.failures != 0 {
			t.Fatalf("successful evidence path changed: %v", err)
		}
	})
	path := filepath.Join(local02Output(t), "performance-checks.json")
	const original = "preserve earlier evidence\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	reporter := &local02ErrorRecorder{}
	recordPerformanceEvidence(reporter, path, map[string]string{"P7": "NOT_RUN"})
	if reporter.failures != 1 {
		t.Error("mandatory evidence write failed without reporting a test failure")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatal("existing failure evidence was overwritten")
	}
}

func TestLocal02PerformancePreservesPrimaryFailure(t *testing.T) {
	for _, kind := range []string{"mode", "soak"} {
		for _, occupied := range []bool{false, true} {
			name := kind + "-available"
			if occupied {
				name = kind + "-occupied"
			}
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(local02Output(t), "failed-window.json")
				if occupied {
					if err := os.WriteFile(path, []byte("older evidence"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				reporter := &local02ErrorRecorder{}
				checks := map[string]any{}
				primary := adapter.ErrBudget
				sample := local02ResourceSample()
				sample.CPUPercent = 81
				measured := measuredRound{Mode: kind, ResourceSamples: []adapter.ResourceSample{sample}}
				key := "P7"
				if kind == "soak" {
					key = "P6"
					recordSoakWindow(reporter, path, measured, checks, primary)
				} else {
					failPerformanceRound(reporter, path, measured, checks, key, primary)
				}
				if len(reporter.fatalErrors) != 1 || !errors.Is(reporter.fatalErrors[0],
					primary) || checks[key] != "FAIL" {
					t.Errorf("primary failure lost or replaced: errors=%v checks=%v", reporter.fatalErrors, checks)
				}
				if occupied {
					if reporter.failures != 1 {
						t.Error("secondary evidence failure was not separately reported")
					}
					got, err := os.ReadFile(path)
					if err != nil || string(got) != "older evidence" {
						t.Fatal("older evidence overwritten")
					}
					return
				}
				var saved map[string]json.RawMessage
				if err := readJSONFile(path, &saved); err != nil {
					t.Fatal(err)
				}
				var cause string
				if err := json.Unmarshal(saved["failure_cause"], &cause); err != nil || cause != primary.Error() {
					t.Error("primary cause absent from persisted failure evidence")
				}
				var samples []adapter.ResourceSample
				if err := json.Unmarshal(saved["ResourceSamples"],
					&samples); err != nil || len(samples) != 1 || samples[0].CPUPercent != 81 {
					t.Fatal("stopping resource sample absent from persisted evidence")
				}
			})
		}
	}
}

func local02DisableReal(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PRIMIND_NG_E2_ENABLE", "PRIMIND_NG_PERF_ENABLE", "PRIMIND_NG_C1_WORKER",
		"PRIMIND_NG_C1_BARRIER", "PRIMIND_NG_ENVIRONMENT_STAGE", "PRIMIND_NG_USERNAME", "PRIMIND_NG_PASSWORD",
		"PRIMIND_NG_ADDRESSES", "PRIMIND_NG_TLS", "PRIMIND_NG_CA_FILE", "PRIMIND_NG_SERVER_NAME", "PRIMIND_NG_OUTPUT",
		"PRIMIND_NEO4J_TEST_URI", "LOG_PATH",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("PRIMIND_NG_PROFILE", filepath.Join(local02Output(t), "not-created-profile.json"))
	t.Setenv("LLM_DEBUG_LOG", "false")
}

func TestLocal02EntrypointsDefaultDisabled(t *testing.T) {
	local02DisableReal(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary,
		"-test.run=^TestNebula(Performance|RealContract|C1Worker|FunctionalEnvironment)$", "-test.v",
		"-test.timeout=10s")
	output, err := cmd.CombinedOutput()
	if saveErr := os.WriteFile(filepath.Join(local02Output(t), "disabled-entries.log"), output, 0o600); saveErr != nil {
		t.Fatal(saveErr)
	}
	if err != nil {
		t.Fatalf("disabled entry process: %v\n%s", err, output)
	}
	for _, name := range []string{"Performance", "RealContract", "C1Worker", "FunctionalEnvironment"} {
		if !strings.Contains(string(output), "--- SKIP: TestNebula"+name+" ") {
			t.Fatalf("entry did not stop before profile/network access: %s\n%s", name, output)
		}
	}
}

func TestLocal02C1EarlyExitDrainsChildren(t *testing.T) {
	local02DisableReal(t)
	out := local02Output(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := runC1(ctx, out, 0)
	if err == nil || !strings.Contains(err.Error(), "child exited before barrier") {
		t.Fatalf("missing early-exit failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "c1-00.start")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("barrier released after child exit")
	}
	for _, name := range []string{"c1-00-0.log", "c1-00-1.log"} {
		path := filepath.Join(out, name)
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
		// runC1 must have reaped both children and closed both owned log handles.
		if err := os.Rename(path, path+".closed"); err != nil {
			t.Fatal("child log remains held:", err)
		}
		if err := os.Rename(path+".closed", path); err != nil {
			t.Fatal(err)
		}
	}
}

func local04Output(t *testing.T) string {
	t.Helper()
	root := os.Getenv("PRIMIND_LOCAL04_ARTIFACTS")
	if root == "" {
		return t.TempDir()
	}
	if !filepath.IsAbs(root) {
		t.Fatal("LOCAL-04 evidence path must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "local04-")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Fixed logical clocks and literal samples below are offline controls. No
// adapter, service, sampler, sleep or measured performance is involved.
func TestLocal04SoakEvidence(t *testing.T) {
	for _, mode := range []string{
		"normal", "reads-not-writes", "failed-window", "counter-backwards", "dropped-samples",
		"probe-failure", "reconcile-failure", "attempt-budget",
	} {
		t.Run(mode, func(t *testing.T) {
			dir := local04Output(t)
			p := adapter.RunProfile{Calls: 3, MinCalls: 3, MaxSoakCalls: 6, MinBatches: 10, SoakSeconds: 1}
			if mode == "attempt-budget" {
				p.SoakSeconds, p.MaxSoakCalls = 2, 3
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
			now := base
			current := adapter.Metrics{StatementCalls: 100, SDKReconnects: 2}
			windows, reconciles := 0, 0
			primary := errors.New("synthetic local04 primary failure")
			h := soakHooks{
				Now:         func() time.Time { return now },
				Metrics:     func() adapter.Metrics { return current },
				TakeSamples: func() { current.Samples = nil },
				Replay: func(context.Context) (measuredRound, error) {
					windows++
					now = now.Add(500 * time.Millisecond)
					samples := []adapter.Metric{}
					for _, kind := range []string{
						"primind_entity", "primind_relation", "primind_source",
						"primind_from", "primind_to", "primind_owner",
					} {
						samples = append(samples, adapter.Metric{Kind: kind, Records: 1})
					}
					samples = append(samples, adapter.Metric{Kind: "search"}, adapter.Metric{Kind: "snapshot"})
					if mode == "reads-not-writes" {
						now = now.Add(500 * time.Millisecond)
						samples = []adapter.Metric{{Kind: "primind_entity", Records: 1}}
						for i := 0; i < 99; i++ {
							samples = append(samples, adapter.Metric{Kind: "search"})
						}
					}
					current.StatementCalls += uint64(len(samples))
					if windows == 1 {
						current.SDKReconnects++
					} // Legal reconnect, no execution failure.
					var err error
					if mode == "failed-window" {
						samples[0].Failed, samples[0].ErrorCode = true, -1
						current.Errors++
						err = primary
					}
					if mode == "counter-backwards" {
						current.StatementCalls = 99
					}
					if mode == "dropped-samples" {
						current.DroppedSamples = 1
					}
					current.Samples = samples
					m := measuredRound{Mode: "soak", Calls: 3, Metrics: current, ReadLatenciesNS: []int64{1}}
					if err != nil {
						m.Errors = 1
					}
					return m, err
				},
				Probe: func(context.Context) error {
					current.StatementCalls++
					if mode == "probe-failure" {
						return primary
					}
					return nil
				},
				Reconcile: func(context.Context) error {
					reconciles++
					now = now.Add(9 * time.Second)
					current.StatementCalls += 50 // Must not contaminate the frozen load snapshot.
					if mode == "reconcile-failure" {
						return primary
					}
					return nil
				},
			}
			err := runSoak(ctx, p, "synthetic_local04", dir, h)
			if (err == nil) != (mode == "normal") {
				t.Errorf("unexpected outcome: %v", err)
			}
			if mode == "failed-window" || mode == "probe-failure" || mode == "reconcile-failure" {
				if !errors.Is(err, primary) {
					t.Error("primary failure was lost", err)
				}
			}
			var got soakEvidence
			if readErr := readJSONFile(filepath.Join(dir, "soak.json"), &got); readErr != nil {
				t.Fatal("summary missing, including on failure:", readErr)
			}
			if got.Format != "NG_SOAK_EVIDENCE_V2" || got.Run != "synthetic_local04" ||
				got.Before.StatementCalls != 100 || len(got.Windows) != windows {
				t.Fatalf("identity/baseline/window evidence incomplete: %+v", got)
			}
			if (got.Status == "MEASURED") != (mode == "normal") || (mode != "normal" && got.FailureCause == "") {
				t.Error("failure was recorded as success or lost", got.Status, got.FailureCause)
			}
			if mode == "normal" || mode == "reconcile-failure" {
				if windows != 2 || got.Calls != 6 || got.ReadProbes != 2 || got.SuccessfulWriteBatches != 12 ||
					got.Metrics.StatementCalls != 118 || got.CounterDelta["statement_calls"] == nil ||
					*got.CounterDelta["statement_calls"] != 18 || got.CounterDelta["sdk_reconnects"] == nil ||
					*got.CounterDelta["sdk_reconnects"] != 1 {
					t.Fatal("literal multi-window totals differ", got)
				}
				if got.LoadDurationNS != int64(time.Second) || got.ReconcileDurationNS == nil ||
					*got.ReconcileDurationNS != int64(9*time.Second) || got.DurationNS != int64(10*time.Second) ||
					got.ReconcileStartedAt == nil || !got.LoadEndedAt.Equal(*got.ReconcileStartedAt) {
					t.Fatal("load/reconcile/legacy total boundaries differ", got)
				}
				wantReconcile := "PASS"
				if mode == "reconcile-failure" {
					wantReconcile = "FAIL"
				}
				if got.ReconcileStatus != wantReconcile {
					t.Error("reconcile result lost", got.ReconcileStatus)
				}
			} else if reconciles != 0 || got.ReconcileStatus != "NOT_RUN" || got.ReconcileStartedAt != nil ||
				got.ReconcileDurationNS != nil {
				t.Error("reconcile ran after an invalid load")
			}
			if mode == "reads-not-writes" && (got.SuccessfulWriteBatches != 1 || got.Metrics.StatementCalls != 201 ||
				got.CounterDelta["statement_calls"] == nil || *got.CounterDelta["statement_calls"] != 101) {
				t.Error("reads inflated successful writes", got)
			}
			if mode == "counter-backwards" && (got.CounterDelta["statement_calls"] != nil ||
				got.Metrics.StatementCalls != 99) {
				t.Error("counter rollback fabricated a positive delta", got)
			}
			for i, name := range got.Windows {
				var window soakWindowEvidence
				if readErr := readJSONFile(filepath.Join(dir, name), &window); readErr != nil {
					t.Fatal(readErr)
				}
				if window.RunID != got.Run || window.Window != i+1 || window.Mode != "soak" ||
					window.LoadDurationNS <= 0 {
					t.Error("window identity/time missing", window)
				}
				if mode == "normal" && (window.SuccessfulWriteBatches != 6 ||
					window.Before.StatementCalls != uint64(100+9*i) ||
					window.Metrics.StatementCalls != uint64(108+9*i) || window.CounterDelta["statement_calls"] == nil ||
					*window.CounterDelta["statement_calls"] != 8) {
					t.Error("window count/baseline mismatch", window)
				}
				if mode == "failed-window" && (window.SuccessfulWriteBatches != 5 || window.FailureCause == "" ||
					!window.Metrics.Samples[0].Failed) {
					t.Error("failed batch was counted or lost", window)
				}
			}
		})
	}
}

func TestLocal04ShortLoadCannotBorrowReconcileTime(t *testing.T) {
	dir := local04Output(t)
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	now := base.Add(500 * time.Millisecond)
	e := soakEvidence{
		Run: "synthetic_short", LoadStartedAt: base, LoadEndedAt: now, Calls: 6, ReadProbes: 2,
		SuccessfulWriteBatches: 12, Before: adapter.Metrics{StatementCalls: 100},
		Metrics: adapter.Metrics{StatementCalls: 118},
	}
	reconciles := 0
	h := soakHooks{
		Now:       func() time.Time { return now },
		Reconcile: func(context.Context) error { reconciles++; now = now.Add(9 * time.Second); return nil },
	}
	p := adapter.RunProfile{SoakSeconds: 1, MinCalls: 3, MaxSoakCalls: 6, MinBatches: 10}
	err := finishSoak(context.Background(), p, dir, &e, h, nil)
	if err == nil || reconciles != 0 {
		t.Error("short load accepted or extended by slow reconciliation", err)
	}
	var saved soakEvidence
	if err := readJSONFile(filepath.Join(dir, "soak.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.LoadDurationNS != int64(500*time.Millisecond) || saved.DurationNS != int64(500*time.Millisecond) ||
		saved.ReconcileStatus != "NOT_RUN" || saved.Status != "FAIL" {
		t.Fatal("short-load failure boundary not retained", saved)
	}
}

func TestLocal04SoakWriteFailurePreservesPrimary(t *testing.T) {
	dir := local04Output(t)
	path := filepath.Join(dir, "soak.json")
	const older = "preserve occupied summary"
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	e := soakEvidence{
		Run: "synthetic_save_failure", LoadStartedAt: base, LoadEndedAt: base.Add(time.Second), Calls: 6,
		ReadProbes: 2, SuccessfulWriteBatches: 12, Before: adapter.Metrics{StatementCalls: 100},
		Metrics: adapter.Metrics{StatementCalls: 118},
	}
	p := adapter.RunProfile{SoakSeconds: 1, MinCalls: 3, MaxSoakCalls: 6, MinBatches: 10}
	primary := errors.New("synthetic primary before save")
	h := soakHooks{
		Now:       func() time.Time { return base.Add(time.Second) },
		Reconcile: func(context.Context) error { t.Fatal("reconcile after failure"); return nil },
	}
	err := finishSoak(context.Background(), p, dir, &e, h, primary)
	if !errors.Is(err, primary) || !errors.Is(err, os.ErrExist) {
		t.Fatal("primary or evidence-save failure was lost", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != older {
		t.Fatal("occupied summary was overwritten")
	}
}
