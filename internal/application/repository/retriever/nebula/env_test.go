package nebula

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func lookupMap(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func baseEnv() map[string]string {
	return map[string]string{
		"NEBULA_ADDRS": "127.0.0.1:9669, 127.0.0.2:9669", "NEBULA_SPACE": "primind_ng_app",
		"NEBULA_USERNAME": "root", "NEBULA_PASSWORD": "pw",
	}
}

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv(lookupMap(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Addresses) != 2 || cfg.Addresses[1] != "127.0.0.2:9669" || cfg.Space != "primind_ng_app" {
		t.Fatalf("unexpected endpoint config: %+v", cfg.Addresses)
	}
	def := DefaultConfig()
	if cfg.Connections != def.Connections || cfg.CloseTimeout != def.CloseTimeout || !cfg.AutoSchema {
		t.Fatal("defaults not applied")
	}
	if strings.Contains(cfg.Summary(), "pw") {
		t.Fatal("summary leaks password")
	}
}

func TestConfigFromEnvOverridesEveryBudget(t *testing.T) {
	env := baseEnv()
	for k, v := range map[string]string{
		"NEBULA_CONNECTIONS": "8", "NEBULA_WRITES": "3", "NEBULA_WAITERS": "16",
		"NEBULA_PENDING_BYTES": "4194304", "NEBULA_PLAN_BYTES": "16777216", "NEBULA_BATCH_RECORDS": "50",
		"NEBULA_STATEMENT_BYTES": "32768", "NEBULA_OPERATION_BYTES": "4194304", "NEBULA_SNAPSHOT_RECORDS": "500",
		"NEBULA_READ_BYTES": "1048576", "NEBULA_VID_BYTES": "512",
		"NEBULA_ACQUIRE_TIMEOUT": "2s", "NEBULA_SOCKET_TIMEOUT": "3s", "NEBULA_OPERATION_TIMEOUT": "10s",
		"NEBULA_CLOSE_TIMEOUT": "6s", "NEBULA_READY_TIMEOUT": "7s", "NEBULA_POLL_INTERVAL": "50ms",
		"NEBULA_AUTO_SCHEMA": "false", "NEBULA_TLS": "true", "NEBULA_TLS_SERVER_NAME": "graphd",
	} {
		env[k] = v
	}
	cfg, err := ConfigFromEnv(lookupMap(env))
	if err != nil {
		t.Fatal(err)
	}
	got := []int{
		cfg.Connections, cfg.Writes, cfg.Waiters, cfg.PendingBytes, cfg.PlanBytes, cfg.BatchRecords,
		cfg.StatementBytes, cfg.OperationBytes, cfg.SnapshotRecords, cfg.ReadBytes, cfg.VIDBytes,
	}
	want := []int{8, 3, 16, 4 << 20, 16 << 20, 50, 32 << 10, 4 << 20, 500, 1 << 20, 512}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("int field %d = %d, want %d", i, got[i], want[i])
		}
	}
	gotD := []time.Duration{
		cfg.AcquireTimeout, cfg.SocketTimeout, cfg.OperationTimeout, cfg.CloseTimeout,
		cfg.ReadyTimeout, cfg.PollInterval,
	}
	wantD := []time.Duration{
		2 * time.Second, 3 * time.Second, 10 * time.Second, 6 * time.Second, 7 * time.Second,
		50 * time.Millisecond,
	}
	for i := range wantD {
		if gotD[i] != wantD[i] {
			t.Fatalf("duration field %d = %v, want %v", i, gotD[i], wantD[i])
		}
	}
	if cfg.AutoSchema || !cfg.TLS || cfg.ServerName != "graphd" {
		t.Fatal("bool/TLS fields not applied")
	}
}

func TestConfigFromEnvErrors(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"missing addrs":    func(e map[string]string) { delete(e, "NEBULA_ADDRS") },
		"missing password": func(e map[string]string) { delete(e, "NEBULA_PASSWORD") },
		"bad space":        func(e map[string]string) { e["NEBULA_SPACE"] = "1bad-space" },
		"bad int":          func(e map[string]string) { e["NEBULA_CONNECTIONS"] = "four" },
		"bad duration":     func(e map[string]string) { e["NEBULA_SOCKET_TIMEOUT"] = "5" },
		"bad bool":         func(e map[string]string) { e["NEBULA_TLS"] = "maybe" },
		"limits violated":  func(e map[string]string) { e["NEBULA_WRITES"] = "4" },
		"close too short":  func(e map[string]string) { e["NEBULA_CLOSE_TIMEOUT"] = "1s" },
	} {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			mutate(env)
			_, err := ConfigFromEnv(lookupMap(env))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
			if strings.Contains(err.Error(), "pw") {
				t.Fatalf("error leaks password: %v", err)
			}
		})
	}
}
