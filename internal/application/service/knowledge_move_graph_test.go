package service

import (
	"context"
	"testing"

	"github.com/primind/primind/internal/types"
)

// A reuse_vectors move deletes the source graph namespace; the moved chunks
// must be re-extracted under the target KB, one task per text chunk, and only
// when the target extracts graphs and the graph backend is on.
func TestEnqueueMovedGraphExtract(t *testing.T) {
	graphKB := &types.KnowledgeBase{
		ID: "kb-target", SummaryModelID: "m1",
		IndexingStrategy: types.IndexingStrategy{GraphEnabled: true},
		ExtractConfig:    &types.ExtractConfig{Enabled: true},
	}
	noGraphKB := &types.KnowledgeBase{ID: "kb-plain", IndexingStrategy: types.IndexingStrategy{VectorEnabled: true}}
	chunks := []*types.Chunk{
		{ID: "c1", ChunkType: types.ChunkTypeText, Content: "Alice knows Bob."},
		{ID: "c2", ChunkType: types.ChunkTypeText, Content: "Alice knows Bob."},
		{ID: "c3", ChunkType: types.ChunkTypeText, Content: "Alice knows Bob."},
	}
	knowledge := &types.Knowledge{ID: "k1", TenantID: 1}

	cases := []struct {
		name    string
		backend string
		target  *types.KnowledgeBase
		want    int
	}{
		{"graph target, nebulagraph", types.GraphBackendNebula, graphKB, len(chunks)},
		{"graph target, backend disabled", types.GraphBackendDisabled, graphKB, 0},
		{"target without extraction", types.GraphBackendNebula, noGraphKB, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGraphBackendForTest(t, tc.backend)
			enq := &countingEnqueuer{}
			svc := &knowledgeService{task: enq}
			got := svc.enqueueTransferredGraphExtract(context.Background(), knowledge, tc.target, chunks)
			if got != tc.want || enq.calls != tc.want {
				t.Fatalf("enqueued=%d calls=%d, want %d", got, enq.calls, tc.want)
			}
		})
	}
}

// A KB copy must keep the graph extraction settings, otherwise the cloned
// chunks are never extracted and the copy silently has no graph.
func TestCloneExtractConfigCopiesWithoutAliasing(t *testing.T) {
	if cloneExtractConfig(nil) != nil {
		t.Fatal("nil config must stay nil")
	}
	src := &types.ExtractConfig{Enabled: true, Text: "t", Tags: []string{"承担"}}
	out := cloneExtractConfig(src)
	if out == src || !out.Enabled || out.Text != "t" || len(out.Tags) != 1 {
		t.Fatalf("clone = %+v", out)
	}
	out.Tags[0] = "changed"
	if src.Tags[0] != "承担" {
		t.Fatal("clone aliases the source tags")
	}
	copied := &types.KnowledgeBase{
		IndexingStrategy: types.IndexingStrategy{GraphEnabled: true},
		ExtractConfig:    out,
	}
	if !copied.IsGraphEnabled() {
		t.Fatal("copied KB must keep graph extraction enabled")
	}
}
