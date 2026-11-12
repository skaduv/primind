package nebula

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EnvConfig is the application-level NebulaGraph configuration read from
// NEBULA_* environment variables. Config itself never reads the environment.
type EnvConfig struct {
	Config
	// AutoSchema creates tags/edges/indexes when the dedicated Space exists but
	// is still empty. A non-empty incompatible Space is always an error.
	AutoSchema bool
}

// ConfigFromEnv builds an EnvConfig from NEBULA_* variables on top of
// DefaultConfig. Required: NEBULA_ADDRS, NEBULA_SPACE, NEBULA_USERNAME,
// NEBULA_PASSWORD. Every budget and timeout in Config can be overridden.
func ConfigFromEnv(lookup func(string) (string, bool)) (EnvConfig, error) {
	get := func(k string) string { v, _ := lookup(k); return strings.TrimSpace(v) }
	cfg := EnvConfig{Config: DefaultConfig()}
	for _, a := range strings.Split(get("NEBULA_ADDRS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.Addresses = append(cfg.Addresses, a)
		}
	}
	cfg.Space = get("NEBULA_SPACE")
	cfg.Username = get("NEBULA_USERNAME")
	cfg.Password, _ = lookup("NEBULA_PASSWORD")
	cfg.ServerName = get("NEBULA_TLS_SERVER_NAME")

	var errs []string
	boolVar := func(k string, dst *bool) {
		if v := get(k); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, k)
				return
			}
			*dst = b
		}
	}
	boolVar("NEBULA_TLS", &cfg.TLS)
	cfg.AutoSchema = true
	boolVar("NEBULA_AUTO_SCHEMA", &cfg.AutoSchema)
	if ca := get("NEBULA_TLS_CA_FILE"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			errs = append(errs, "NEBULA_TLS_CA_FILE")
		}
		cfg.CACertificatePEM = string(pem)
	}
	for k, dst := range map[string]*int{
		"NEBULA_CONNECTIONS":      &cfg.Connections,
		"NEBULA_WRITES":           &cfg.Writes,
		"NEBULA_WAITERS":          &cfg.Waiters,
		"NEBULA_PENDING_BYTES":    &cfg.PendingBytes,
		"NEBULA_PLAN_BYTES":       &cfg.PlanBytes,
		"NEBULA_BATCH_RECORDS":    &cfg.BatchRecords,
		"NEBULA_STATEMENT_BYTES":  &cfg.StatementBytes,
		"NEBULA_OPERATION_BYTES":  &cfg.OperationBytes,
		"NEBULA_SNAPSHOT_RECORDS": &cfg.SnapshotRecords,
		"NEBULA_READ_BYTES":       &cfg.ReadBytes,
		"NEBULA_VID_BYTES":        &cfg.VIDBytes,
	} {
		if v := get(k); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, k)
				continue
			}
			*dst = n
		}
	}
	for k, dst := range map[string]*time.Duration{
		"NEBULA_ACQUIRE_TIMEOUT":     &cfg.AcquireTimeout,
		"NEBULA_SOCKET_TIMEOUT":      &cfg.SocketTimeout,
		"NEBULA_OPERATION_TIMEOUT":   &cfg.OperationTimeout,
		"NEBULA_CLOSE_TIMEOUT":       &cfg.CloseTimeout,
		"NEBULA_READY_TIMEOUT":       &cfg.ReadyTimeout,
		"NEBULA_POLL_INTERVAL":       &cfg.PollInterval,
		"NEBULA_SEARCH_SCAN_TIMEOUT": &cfg.SearchScanTimeout,
	} {
		if v := get(k); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, k)
				continue
			}
			*dst = d
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return cfg, fmt.Errorf("%w: unparsable %s", ErrInvalid, strings.Join(errs, ", "))
	}
	if err := cfg.validate(); err != nil {
		return cfg, fmt.Errorf("NEBULA_* configuration: %w", err)
	}
	return cfg, nil
}

// Summary describes the configuration for logs without credentials.
func (c EnvConfig) Summary() string {
	return fmt.Sprintf("addrs=%v space=%s tls=%t connections=%d writes=%d batch=%d",
		c.Addresses, c.Space, c.TLS, c.Connections, c.Writes, c.BatchRecords)
}
