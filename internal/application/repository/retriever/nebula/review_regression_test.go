package nebula

import (
	"context"
	"fmt"
	"strings"
	"testing"

	ng "github.com/vesoft-inc/nebula-go/v3"
	nv "github.com/vesoft-inc/nebula-go/v3/nebula"
	gr "github.com/vesoft-inc/nebula-go/v3/nebula/graph"
)

// These are coordinator review assertions on an exported fixed commit, not E2.
func TestCoordinatorInvalidSessionsDoNotPoisonPool(t *testing.T) {
	for _, code := range []nv.ErrorCode{nv.ErrorCode_E_SESSION_INVALID, nv.ErrorCode_E_SESSION_TIMEOUT} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			badResult, _ := ng.GenResultSet(&gr.ExecutionResponse{ErrorCode: code})
			goodResult, _ := ng.GenResultSet(&gr.ExecutionResponse{ErrorCode: nv.ErrorCode_SUCCEEDED})
			first := &protocolSession{result: badResult}
			second := &protocolSession{result: badResult}
			d := &sdkDriver{cfg: testConfig(), sessions: make(chan sdkSession, 2)}
			d.sessions <- first
			d.sessions <- second
			created := 0
			d.create = func(context.Context) (sdkSession, error) {
				created++
				return &protocolSession{result: goodResult}, nil
			}
			defer d.Close()
			for i := 0; i < 2; i++ {
				out := d.Execute(context.Background(), "synthetic")
				if out.Err == nil || !out.Sent || created != 0 {
					t.Fatal("original invalid-session request must return its error without automatic replay")
				}
			}
			for i := 0; i < 2; i++ {
				out := d.Execute(context.Background(), "next-independent-call")
				if out.Err != nil {
					t.Errorf("new call reused invalid session: %v", out.Err)
				}
			}
			if first.releases.Load() != 1 || second.releases.Load() != 1 || created != 2 {
				t.Errorf("pool remains poisoned: releases=%d/%d new_sessions=%d", first.releases.Load(), second.releases.Load(), created)
			}
		})
	}
}

func TestCoordinatorPerformanceRequiresConcurrentConfiguration(t *testing.T) {
	p := RunProfile{Status: "AUTHORIZED", AuthorizationRef: "local-validation-only", RunID: "review", Space: "review", Version: "3.8.0", SDK: "v3.8.0", SourceCommit: strings.Repeat("a", 40), Instances: 1, Connections: 4, C1ConnectionBudget: 12, MaxConcurrency: 2, C1Rounds: 3, MaxDurationSeconds: 3600, DrainSeconds: 30, StatementBytes: 65536, Calls: 20, GroupPairs: 1, MaxSoakCalls: 1000, MaxPhysicalRecords: 500, Rounds: 3, SoakSeconds: 10, MinBatches: 10, MinCalls: 10, BatchSizes: []int{50, 200}, Concurrency: []int{1, 2}, ResourceSamplePath: "not-opened.json", MaxCPUPercent: 60, MaxMemoryBytes: 1 << 30, MaxDiskIOBytesPerSecond: 1 << 30, MinFreeDiskBytes: 1 << 30, MaxServiceLatencyMS: 100, MaxServiceErrorRate: .01, AllowWrite: true, AllowCleanup: true}
	p.Environment = map[string]string{}
	for _, key := range []string{"cpu", "memory_limit", "disk_type", "disk_available", "network", "schema", "index", "partitions", "replicas", "background_load", "window", "resource_sampler"} {
		p.Environment[key] = "synthetic validation only"
	}
	if err := p.Validate(true); err != nil {
		t.Fatalf("control profile rejected: %v", err)
	}
	p.Concurrency = []int{1, 1}
	if err := p.Validate(true); err == nil {
		t.Fatal("duplicate serial-only matrix accepted; P7 requires serial and concurrent configurations")
	}
	// The first matrix row is the serial baseline. The runner uses the final
	// batch/concurrency pair for soak; unique rows keep that selected row > 1.
	p.Connections, p.C1ConnectionBudget, p.MaxConcurrency = 5, 15, 3
	for _, tc := range []struct {
		name   string
		levels []int
		valid  bool
	}{
		{"serial_and_parallel", []int{1, 2}, true},
		{"multiple_parallel_levels", []int{1, 2, 3}, true},
		{"explicit_nonascending_selection", []int{1, 3, 2}, true},
		{"only_serial", []int{1}, false},
		{"repeated_serial", []int{1, 1}, false},
		{"repeated_parallel", []int{1, 2, 2}, false},
		{"serial_soak_selection", []int{1, 2, 1}, false},
		{"missing_serial", []int{2, 3}, false},
		{"serial_not_first", []int{2, 1}, false},
		{"over_budget", []int{1, 4}, false},
		{"zero", []int{1, 0}, false},
		{"empty", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := p
			candidate.Concurrency = tc.levels
			if err := candidate.Validate(true); (err == nil) != tc.valid {
				t.Fatalf("matrix=%v valid=%v err=%v", tc.levels, tc.valid, err)
			}
		})
	}
}
