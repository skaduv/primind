package nebula

import (
	"errors"
	"fmt"
	"time"
)

var ErrAuthorization = errors.New("isolated resource and load authorization is missing")

// RunProfile is safety metadata, not a credential/configuration file. A completed
// profile references the coordinator's authorization; a template cannot execute.
type RunProfile struct {
	Status                  string            `json:"status"`
	AuthorizationRef        string            `json:"authorization_ref"`
	RunID                   string            `json:"run_id"`
	Space                   string            `json:"space"`
	Version                 string            `json:"service_version"`
	SDK                     string            `json:"sdk_version"`
	SourceCommit            string            `json:"source_commit"`
	Environment             map[string]string `json:"environment"`
	Instances               int               `json:"instances"`
	Seed                    int64             `json:"seed"`
	Calls                   int               `json:"calls"`
	GroupPairs              int               `json:"group_pairs"`
	MaxSoakCalls            int               `json:"max_soak_calls"`
	MaxPhysicalRecords      int               `json:"max_physical_records"`
	MaxConcurrency          int               `json:"max_concurrency"`
	Connections             int               `json:"connections"`
	C1ConnectionBudget      int               `json:"c1_connection_budget"`
	Rounds                  int               `json:"rounds"`
	BatchSizes              []int             `json:"batch_sizes"`
	Concurrency             []int             `json:"concurrency"`
	StatementBytes          int               `json:"statement_bytes"`
	MaxDurationSeconds      int               `json:"max_duration_seconds"`
	DrainSeconds            int               `json:"drain_seconds"`
	SoakSeconds             int               `json:"soak_seconds"`
	MinBatches              int               `json:"min_batches"`
	MinCalls                int               `json:"min_calls"`
	C1Rounds                int               `json:"c1_rounds"`
	AllowWrite              bool              `json:"allow_write"`
	AllowCleanup            bool              `json:"allow_cleanup"`
	CreateEmptySchema       bool              `json:"create_empty_schema"`
	ResourceSamplePath      string            `json:"resource_sample_path"`
	MaxCPUPercent           float64           `json:"max_cpu_percent"`
	MaxMemoryBytes          uint64            `json:"max_memory_bytes"`
	MaxDiskIOBytesPerSecond float64           `json:"max_disk_io_bytes_per_second"`
	MinFreeDiskBytes        uint64            `json:"min_free_disk_bytes"`
	MaxServiceLatencyMS     float64           `json:"max_service_latency_ms"`
	MaxServiceErrorRate     float64           `json:"max_service_error_rate"`
	BusinessTargetStatus    string            `json:"business_target_status"`
}

func (p RunProfile) Validate(performance bool) error {
	if p.Status != "AUTHORIZED" || p.AuthorizationRef == "" || !p.AllowWrite || !p.AllowCleanup {
		return ErrAuthorization
	}
	if !schemaName.MatchString(p.RunID) || len(p.RunID) > 40 || !schemaName.MatchString(p.Space) || p.Version != "3.8.0" || p.SDK != "v3.8.0" || len(p.SourceCommit) != 40 {
		return fmt.Errorf("%w: version/run/space/source", ErrInvalid)
	}
	if p.Instances != 1 || p.Connections < 4 || p.Connections > 32 || p.MaxConcurrency < 2 || p.MaxConcurrency >= p.Connections-1 || p.C1ConnectionBudget < 3*p.Connections || p.C1Rounds < 3 || p.C1Rounds > 20 {
		return fmt.Errorf("%w: instance/connection/concurrency/C1 budget", ErrInvalid)
	}
	if p.MaxDurationSeconds < 1 || p.MaxDurationSeconds > 7200 || p.DrainSeconds < 1 || p.DrainSeconds > 300 || p.StatementBytes < 1024 || p.StatementBytes > 4<<20 {
		return ErrBudget
	}
	if !performance {
		return nil
	}
	for _, key := range []string{"cpu", "memory_limit", "disk_type", "disk_available", "network", "schema", "index", "partitions", "replicas", "background_load", "window", "resource_sampler"} {
		if p.Environment[key] == "" {
			return fmt.Errorf("%w: environment %s", ErrAuthorization, key)
		}
	}
	if p.Calls < 2 || p.Calls > 1000 || p.GroupPairs < 1 || p.GroupPairs > 200 || p.Seed < 0 || p.MaxSoakCalls < p.MinCalls || p.MaxSoakCalls > 1000000 || p.MaxPhysicalRecords < (20*p.GroupPairs+2)*p.Calls || p.MaxPhysicalRecords > 1000000 || p.Rounds < 3 || p.Rounds > 10 || p.SoakSeconds < 1 || p.SoakSeconds >= p.MaxDurationSeconds || p.MinBatches < 10 || p.MinCalls <= p.MaxConcurrency || p.MinCalls > p.Calls {
		return ErrBudget
	}
	if len(p.BatchSizes) < 1 || len(p.BatchSizes) > 4 || len(p.Concurrency) < 2 || len(p.Concurrency) > 4 || p.Concurrency[0] != 1 {
		return ErrBudget
	}
	for _, b := range p.BatchSizes {
		if b < 1 || b > 10000 {
			return ErrBudget
		}
		ceil := func(n int) int { return (n + b - 1) / b }
		if p.Calls*(ceil(2*p.GroupPairs)+2*ceil(3*p.GroupPairs)+3*ceil(4*p.GroupPairs)) > 10000 {
			return fmt.Errorf("%w: per-round metric sample capacity", ErrBudget)
		}
	}
	seenConcurrency := make(map[int]bool, len(p.Concurrency))
	hasConcurrent := false
	for _, c := range p.Concurrency {
		if c < 1 || c > p.MaxConcurrency || seenConcurrency[c] {
			return ErrBudget
		}
		seenConcurrency[c] = true
		hasConcurrent = hasConcurrent || c > 1
	}
	if !hasConcurrent {
		return ErrBudget
	}
	if p.ResourceSamplePath == "" || p.MaxCPUPercent <= 0 || p.MaxCPUPercent > 100 || p.MaxMemoryBytes == 0 || p.MaxDiskIOBytesPerSecond <= 0 || p.MinFreeDiskBytes == 0 || p.MaxServiceLatencyMS <= 0 || p.MaxServiceErrorRate < 0 || p.MaxServiceErrorRate > 1 {
		return ErrAuthorization
	}
	return nil
}

type ResourceSample struct {
	DiskReadBytesPerSecond  float64   `json:"disk_read_bytes_per_second"`
	DiskWriteBytesPerSecond float64   `json:"disk_write_bytes_per_second"`
	At                      time.Time `json:"at"`
	CPUPercent              float64   `json:"cpu_percent"`
	MemoryBytes             uint64    `json:"memory_bytes"`
	FreeDiskBytes           uint64    `json:"free_disk_bytes"`
	ServiceLatencyMS        float64   `json:"service_latency_ms"`
	ServiceErrorRate        float64   `json:"service_error_rate"`
	Connections             int       `json:"connections"`
	Sessions                int       `json:"sessions"`
}

func (p RunProfile) CheckResource(s ResourceSample, now time.Time) error {
	if s.DiskReadBytesPerSecond < 0 || s.DiskWriteBytesPerSecond < 0 || s.CPUPercent < 0 || s.CPUPercent > 100 || s.ServiceLatencyMS < 0 || s.ServiceErrorRate < 0 || s.ServiceErrorRate > 1 || s.Connections < 0 || s.Sessions < 0 {
		return ErrInvalid
	}
	age := now.Sub(s.At)
	if age < 0 || age > 10*time.Second {
		return fmt.Errorf("%w: stale resource sample", ErrAuthorization)
	}
	if s.DiskReadBytesPerSecond+s.DiskWriteBytesPerSecond > p.MaxDiskIOBytesPerSecond || s.CPUPercent > p.MaxCPUPercent || s.MemoryBytes > p.MaxMemoryBytes || s.FreeDiskBytes < p.MinFreeDiskBytes || s.ServiceLatencyMS > p.MaxServiceLatencyMS || s.ServiceErrorRate > p.MaxServiceErrorRate || s.Connections > p.Connections || s.Sessions > p.Connections {
		return fmt.Errorf("%w: resource stop threshold", ErrBudget)
	}
	return nil
}
