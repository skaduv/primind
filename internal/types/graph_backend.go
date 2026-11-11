package types

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Knowledge-graph backends selectable with GRAPH_BACKEND.
const (
	GraphBackendDisabled = "disabled"
	GraphBackendNeo4j    = "neo4j"
	GraphBackendNebula   = "nebulagraph"
)

// ErrGraphDisabled is returned by the disabled graph repository so callers can
// tell "graph off" apart from a successful write.
var ErrGraphDisabled = errors.New("knowledge graph is disabled")

// GraphBackendSelection is the resolved, process-wide graph backend choice.
type GraphBackendSelection struct {
	Backend string
	// Warning is non-empty when GRAPH_BACKEND overrides a conflicting legacy
	// NEO4J_ENABLE value. It never contains credentials.
	Warning string
}

// Enabled reports whether graph extraction and retrieval are on.
func (s GraphBackendSelection) Enabled() bool { return s.Backend != GraphBackendDisabled }

// ResolveGraphBackend applies the selection matrix:
//   - GRAPH_BACKEND unset or empty: legacy rule, neo4j iff
//     strings.ToLower(NEO4J_ENABLE) == "true", otherwise disabled.
//   - GRAPH_BACKEND one of disabled|neo4j|nebulagraph (exact, lowercase): that
//     backend wins; a set NEO4J_ENABLE implying otherwise yields a Warning.
//   - any other value: error, never a silent fallback.
func ResolveGraphBackend(lookup func(string) (string, bool)) (GraphBackendSelection, error) {
	legacy := GraphBackendDisabled
	legacyRaw, legacySet := lookup("NEO4J_ENABLE")
	if strings.ToLower(legacyRaw) == "true" {
		legacy = GraphBackendNeo4j
	}
	raw, _ := lookup("GRAPH_BACKEND")
	if raw == "" {
		return GraphBackendSelection{Backend: legacy}, nil
	}
	switch raw {
	case GraphBackendDisabled, GraphBackendNeo4j, GraphBackendNebula:
	default:
		return GraphBackendSelection{}, fmt.Errorf(
			"invalid GRAPH_BACKEND %q: want one of disabled, neo4j, nebulagraph", raw)
	}
	sel := GraphBackendSelection{Backend: raw}
	if legacySet && legacy != raw {
		sel.Warning = fmt.Sprintf(
			"GRAPH_BACKEND=%s overrides legacy NEO4J_ENABLE (which implies %s)", raw, legacy)
	}
	return sel, nil
}

var (
	graphBackendOnce sync.Once
	graphBackendSel  GraphBackendSelection
	graphBackendErr  error
)

// CurrentGraphBackend resolves GRAPH_BACKEND from the environment once per
// process; every graph coupling point reads this same result. Changes need a
// restart.
func CurrentGraphBackend() (GraphBackendSelection, error) {
	graphBackendOnce.Do(func() {
		graphBackendSel, graphBackendErr = ResolveGraphBackend(os.LookupEnv)
	})
	return graphBackendSel, graphBackendErr
}

// SetGraphBackendForTest overrides the process-wide selection.
func SetGraphBackendForTest(sel GraphBackendSelection) {
	graphBackendOnce.Do(func() {})
	graphBackendSel, graphBackendErr = sel, nil
}

// GraphBackend returns the selected backend; an invalid configuration (which
// already fails container start-up) reads as disabled.
func GraphBackend() string {
	sel, err := CurrentGraphBackend()
	if err != nil {
		return GraphBackendDisabled
	}
	return sel.Backend
}

// GraphRAGEnabled reports whether knowledge-graph extraction and retrieval are on.
func GraphRAGEnabled() bool { return GraphBackend() != GraphBackendDisabled }
