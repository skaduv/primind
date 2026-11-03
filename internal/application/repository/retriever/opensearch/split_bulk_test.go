package opensearch

import (
	"fmt"
	"testing"

	"github.com/primind/primind/internal/types"
)

func splitFixture(n, dim int) ([]*types.IndexInfo, map[string][]float32) {
	infos := make([]*types.IndexInfo, n)
	emb := make(map[string][]float32, n)
	for i := range infos {
		id := fmt.Sprintf("c-%05d", i)
		infos[i] = &types.IndexInfo{ChunkID: id, SourceID: id, KnowledgeID: "k", KnowledgeBaseID: "kb", Content: "text " + id}
		if dim > 0 {
			v := make([]float32, dim)
			for j := range v {
				v[j] = -0.123456789 // worst-case-ish encoded width
			}
			emb[id] = v
		}
	}
	return infos, emb
}

func TestSplitForBulk_EveryGroupFitsBatchSaveLimits(t *testing.T) {
	for _, tc := range []struct{ n, dim int }{{500, 4096}, {500, 768}, {1500, 0}, {3, 16000}} {
		infos, emb := splitFixture(tc.n, tc.dim)
		groups, err := splitForBulk("alias", infos, emb)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, g := range groups {
			vecs := make([][]float32, len(g))
			for i, info := range g {
				vecs[i] = emb[info.SourceID]
			}
			body, err := buildBulkBody("alias", g, vecs, nil)
			if err != nil {
				t.Fatalf("n=%d dim=%d: group of %d rejected: %v", tc.n, tc.dim, len(g), err)
			}
			if len(g) > maxBulkItems || len(body) > maxBulkBytes || estimateBulkBodyBytes(len(g), tc.dim) > maxBulkBytes {
				t.Fatalf("n=%d dim=%d: group of %d exceeds limits (%d bytes)", tc.n, tc.dim, len(g), len(body))
			}
			for _, info := range g {
				if info != infos[seen] {
					t.Fatalf("order/coverage broken at %d", seen)
				}
				seen++
			}
		}
		if seen != tc.n {
			t.Fatalf("n=%d dim=%d: covered %d", tc.n, tc.dim, seen)
		}
		if tc.n == 500 && tc.dim == 4096 && len(groups) < 3 {
			t.Fatalf("500×4096 should need several groups, got %d", len(groups))
		}
	}
}

func TestCopyScanPageSize_FollowsDimensionAndCap(t *testing.T) {
	def := DefaultRuntimeOptions().MaxResponseBytes
	for _, tc := range []struct {
		dim     int
		respCap int64
		want    int
	}{{768, def, copyBatchSize}, {4096, def, 120}, {16000, def, 32}, {4096, 1, 1}} {
		if got := copyScanPageSize(tc.dim, tc.respCap); got != tc.want {
			t.Errorf("dim=%d cap=%d: page %d, want %d", tc.dim, tc.respCap, got, tc.want)
		}
	}
}
