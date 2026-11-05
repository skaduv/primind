package service

import (
	"strings"
	"testing"

	"github.com/primind/primind/internal/types"
)

// Deleting a store keeps its backend data; the hint must name the exact
// OpenSearch prefix the store owns (same rule as the adapter's base index).
func TestVectorStoreLeftoverHint(t *testing.T) {
	os := types.VectorStore{
		ID:               "8896d60e-a609-4235-9f80-5d57e6710e41",
		EngineType:       types.OpenSearchRetrieverEngineType,
		ConnectionConfig: types.ConnectionConfig{Addr: "http://127.0.0.1:9200"},
		IndexConfig:      types.IndexConfig{IndexName: "primind_os_app"},
	}
	hint := vectorStoreLeftoverHint(os)
	for _, want := range []string{"NOT removed", "primind_os_app_8896d60e-a60_*", "http://127.0.0.1:9200"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q missing %q", hint, want)
		}
	}

	other := vectorStoreLeftoverHint(types.VectorStore{ID: "s", EngineType: types.QdrantRetrieverEngineType})
	if !strings.Contains(other, "NOT removed") || strings.Contains(other, "_*") {
		t.Errorf("generic hint = %q", other)
	}
}
