package nebula

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNebulaFunctionalEnvironment is an explicitly enabled, read-only F01
// inventory. It must pass on the empty dedicated Space before Schema creation.
// Postflight captures the retained definitions without deleting any resource.
func TestNebulaFunctionalEnvironment(t *testing.T) {
	stage := os.Getenv("PRIMIND_NG_ENVIRONMENT_STAGE")
	if os.Getenv("PRIMIND_NG_E2_ENABLE") != "authorized-isolated-run" || stage == "" {
		t.Skip("BLOCKED/NOT_RUN: explicit read-only environment inventory absent")
	}
	if stage != "preflight" && stage != "postflight" {
		t.Fatal("invalid environment inventory stage")
	}
	f, err := os.Open(os.Getenv("PRIMIND_NG_PROFILE"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var p RunProfile
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(false); err != nil {
		t.Fatal(err)
	}
	head, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != p.SourceCommit {
		t.Fatal("profile source commit must match checked-out HEAD")
	}
	if p.Space != "primind_ng_contract" || os.Getenv("PRIMIND_NG_ADDRESSES") != "127.0.0.1:19669" {
		t.Fatal("F01 inventory is restricted to the assigned endpoint and contract Space")
	}
	out := os.Getenv("PRIMIND_NG_OUTPUT")
	if !filepath.IsAbs(out) {
		t.Fatal("absolute evidence directory required")
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Addresses, cfg.Space = []string{"127.0.0.1:19669"}, p.Space
	cfg.Username, cfg.Password = os.Getenv("PRIMIND_NG_USERNAME"), os.Getenv("PRIMIND_NG_PASSWORD")
	cfg.TLS, cfg.ServerName = os.Getenv("PRIMIND_NG_TLS") == "true", os.Getenv("PRIMIND_NG_SERVER_NAME")
	if ca := os.Getenv("PRIMIND_NG_CA_FILE"); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		cfg.CACertificatePEM = string(pem)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tables := map[string]wireTable{}
	// Persist even partial inventory on failure; SQL below is fixed metadata SQL.
	t.Cleanup(func() {
		data, err := json.MarshalIndent(map[string]any{
			"stage": stage, "source_commit": p.SourceCommit,
			"space": p.Space, "endpoint": cfg.Addresses[0], "tls": cfg.TLS, "read_only": true, "passed": !t.Failed(),
			"tables": tables,
		}, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		f, err := os.OpenFile(filepath.Join(out, stage+"-environment.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Error(err)
		}
	})
	d, err := newSDKDriver(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	read := func(stmt string) wireTable {
		r := d.Execute(ctx, stmt, nil)
		if r.Err != nil {
			t.Fatalf("read-only inventory %q: %v", stmt, r.Err)
		}
		tables[stmt] = r.Table
		return r.Table
	}
	for _, role := range []string{"GRAPH", "META", "STORAGE"} {
		table := read("SHOW HOSTS " + role + ";")
		if len(table.Rows) == 0 {
			t.Fatal("service inventory empty: " + role)
		}
		for _, row := range table.Rows {
			v, ok := cell(row, fieldIndex(table, "Version"))
			status, hasStatus := cell(row, fieldIndex(table, "Status"))
			if !ok || (v != "3.8.0" && !strings.HasPrefix(v, "3.8.0-")) || !hasStatus || status != "ONLINE" {
				t.Fatal("service version/status incompatible: " + role)
			}
		}
	}
	space := read("DESCRIBE SPACE " + cfg.Space + ";")
	if len(space.Rows) != 1 {
		t.Fatal("Space metadata missing")
	}
	vid, ok := cell(space.Rows[0], fieldIndex(space, "Vid Type"))
	if !ok || strings.ToUpper(vid) != fmt.Sprintf("FIXED_STRING(%d)", cfg.VIDBytes) {
		t.Fatal("Space VID incompatible")
	}
	read("SHOW CREATE SPACE " + cfg.Space + ";")
	for _, stmt := range []string{"SHOW TAGS;", "SHOW EDGES;", "SHOW TAG INDEXES;", "SHOW EDGE INDEXES;"} {
		table := read(stmt)
		if stage == "preflight" && len(table.Rows) != 0 {
			t.Fatal("preflight requires empty dedicated Schema; preserve existing resources: " + stmt)
		}
	}
	if stage == "postflight" {
		for _, name := range definitionNames() {
			kind, _ := definition(name)
			read(fmt.Sprintf("SHOW CREATE %s %s;", kind, name))
			read(fmt.Sprintf("SHOW CREATE %s INDEX %s_scope;", kind, name))
		}
	}
}
