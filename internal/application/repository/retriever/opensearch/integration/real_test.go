//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	secutils "github.com/primind/primind/internal/utils"
)

func realConfig(t *testing.T, mode string) Config {
	t.Helper()
	c, err := loadConfig(mode, os.Getenv)
	if errors.Is(err, errNotRun) {
		t.Skip("NOT_RUN: no explicit authorized configuration; this skip is not E2 PASS")
	}
	if err != nil {
		t.Fatal(err)
	}
	if deadline, ok := t.Deadline(); ok &&
		time.Until(deadline) < time.Duration(c.MaxSeconds)*time.Second+35*time.Second {
		t.Fatal("test timeout must exceed the frozen run budget by at least 35s for bounded drain")
	}
	frozen, _ := json.Marshal(c)
	t.Logf("approved_config_sha256=%s", digest(frozen))
	// No .env loading or inherited proxy/service configuration. Only exact hosts
	// from the approved config are permitted in this standalone test process.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, "")
	}
	u, _ := url.Parse(c.Endpoint)
	hosts := []string{u.Hostname()}
	if c.Performance != nil && c.Performance.Application != nil {
		a, _ := url.Parse(c.Performance.Application.Endpoint)
		hosts = append(hosts, a.Hostname())
	}
	secutils.SetSSRFWhitelistFromRaw(strings.Join(hosts, ","))
	t.Cleanup(func() { secutils.SetSSRFWhitelistFromRaw("") })
	return c
}

func logResult(t *testing.T, result any, err error) {
	t.Helper()
	raw, e := json.Marshal(result)
	if e != nil {
		t.Fatal("result encoding failed")
	}
	t.Log(string(raw))
	if err != nil {
		t.Fatal(err)
	}
}

func TestRealSameChunkIdempotency(t *testing.T) {
	c := realConfig(t, "replay")
	r, e := runReplay(context.Background(), c)
	logResult(t, r, e)
}

func TestRealPerformance(t *testing.T) {
	c := realConfig(t, "performance")
	r, e := runPerformance(context.Background(), c)
	logResult(t, r, e)
}
