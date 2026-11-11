package types

import (
	"strings"
	"testing"
)

func envLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestResolveGraphBackendMatrix(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		warn    bool
		wantErr bool
	}{
		{"legacy true", map[string]string{"NEO4J_ENABLE": "true"}, GraphBackendNeo4j, false, false},
		{"legacy TRUE", map[string]string{"NEO4J_ENABLE": "TRUE"}, GraphBackendNeo4j, false, false},
		{"legacy TrUe", map[string]string{"NEO4J_ENABLE": "TrUe"}, GraphBackendNeo4j, false, false},
		{"legacy false", map[string]string{"NEO4J_ENABLE": "false"}, GraphBackendDisabled, false, false},
		{"legacy empty", map[string]string{"NEO4J_ENABLE": ""}, GraphBackendDisabled, false, false},
		{"legacy other", map[string]string{"NEO4J_ENABLE": "yes"}, GraphBackendDisabled, false, false},
		{
			"legacy padded true stays off",
			map[string]string{"NEO4J_ENABLE": " true "},
			GraphBackendDisabled, false,
			false,
		},
		{"nothing set", map[string]string{}, GraphBackendDisabled, false, false},
		{
			"empty GRAPH_BACKEND is unset",
			map[string]string{"GRAPH_BACKEND": "", "NEO4J_ENABLE": "true"},
			GraphBackendNeo4j, false, false,
		},
		{"disabled", map[string]string{"GRAPH_BACKEND": "disabled"}, GraphBackendDisabled, false, false},
		{
			"disabled overrides legacy true",
			map[string]string{"GRAPH_BACKEND": "disabled", "NEO4J_ENABLE": "true"},
			GraphBackendDisabled, true, false,
		},
		{"neo4j without legacy", map[string]string{"GRAPH_BACKEND": "neo4j"}, GraphBackendNeo4j, false, false},
		{
			"neo4j vs legacy false",
			map[string]string{"GRAPH_BACKEND": "neo4j", "NEO4J_ENABLE": "false"},
			GraphBackendNeo4j, true, false,
		},
		{
			"neo4j agrees with legacy",
			map[string]string{"GRAPH_BACKEND": "neo4j", "NEO4J_ENABLE": "true"},
			GraphBackendNeo4j, false, false,
		},
		{"nebulagraph", map[string]string{"GRAPH_BACKEND": "nebulagraph"}, GraphBackendNebula, false, false},
		{
			"nebulagraph vs legacy true",
			map[string]string{"GRAPH_BACKEND": "nebulagraph", "NEO4J_ENABLE": "true"},
			GraphBackendNebula, true, false,
		},
		{
			"nebulagraph vs legacy false",
			map[string]string{"GRAPH_BACKEND": "nebulagraph", "NEO4J_ENABLE": "false"},
			GraphBackendNebula, true, false,
		},
		{"unknown", map[string]string{"GRAPH_BACKEND": "nebula"}, "", false, true},
		{"uppercase rejected", map[string]string{"GRAPH_BACKEND": "Neo4j"}, "", false, true},
		{"padded rejected", map[string]string{"GRAPH_BACKEND": " neo4j"}, "", false, true},
		{
			"unknown with legacy true",
			map[string]string{"GRAPH_BACKEND": "memgraph", "NEO4J_ENABLE": "true"},
			"", false,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := ResolveGraphBackend(envLookup(tc.env))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", sel)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if sel.Backend != tc.want {
				t.Fatalf("backend = %q, want %q", sel.Backend, tc.want)
			}
			if (sel.Warning != "") != tc.warn {
				t.Fatalf("warning = %q, want present=%v", sel.Warning, tc.warn)
			}
			if sel.Enabled() != (tc.want != GraphBackendDisabled) {
				t.Fatalf("Enabled() = %v for %q", sel.Enabled(), tc.want)
			}
		})
	}
}

func TestResolveGraphBackendWarningHasNoSecrets(t *testing.T) {
	sel, err := ResolveGraphBackend(envLookup(map[string]string{
		"GRAPH_BACKEND": "nebulagraph", "NEO4J_ENABLE": "true", "NEO4J_PASSWORD": "s3cret",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sel.Warning, "s3cret") {
		t.Fatalf("warning leaks credentials: %q", sel.Warning)
	}
}

func TestSetGraphBackendForTestDrivesHelpers(t *testing.T) {
	SetGraphBackendForTest(GraphBackendSelection{Backend: GraphBackendNebula})
	if GraphBackend() != GraphBackendNebula || !GraphRAGEnabled() {
		t.Fatal("nebulagraph selection not visible through helpers")
	}
	SetGraphBackendForTest(GraphBackendSelection{Backend: GraphBackendDisabled})
	if GraphRAGEnabled() {
		t.Fatal("disabled selection reported as enabled")
	}
}
