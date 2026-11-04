package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

// summaryStoreChunkRepo keeps chunks in memory for storeSummaryChunk.
type summaryStoreChunkRepo struct {
	interfaces.ChunkRepository
	chunks map[string]*types.Chunk
}

func (r *summaryStoreChunkRepo) ListChunksByKnowledgeIDAndTypes(
	_ context.Context, _ uint64, knowledgeID string, chunkTypes []types.ChunkType,
) ([]*types.Chunk, error) {
	var out []*types.Chunk
	for _, c := range r.chunks {
		if c.KnowledgeID == knowledgeID && c.ChunkType == chunkTypes[0] {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *summaryStoreChunkRepo) CreateChunks(_ context.Context, chunks []*types.Chunk) error {
	for _, c := range chunks {
		r.chunks[c.ID] = c
	}
	return nil
}

func (r *summaryStoreChunkRepo) UpdateChunk(_ context.Context, c *types.Chunk) error {
	r.chunks[c.ID] = c
	return nil
}

func (r *summaryStoreChunkRepo) DeleteChunk(_ context.Context, _ uint64, id string) error {
	delete(r.chunks, id)
	return nil
}

func (r *summaryStoreChunkRepo) DeleteChunks(_ context.Context, _ uint64, ids []string) error {
	for _, id := range ids {
		delete(r.chunks, id)
	}
	return nil
}

func (r *summaryStoreChunkRepo) summaries() []*types.Chunk {
	var out []*types.Chunk
	for _, c := range r.chunks {
		if c.ChunkType == types.ChunkTypeSummary {
			out = append(out, c)
		}
	}
	return out
}

// An index failure during the summary task must not leave an unindexed
// summary row behind, and the retry must end with exactly one summary chunk.
func TestStoreSummaryChunk_IndexFailureLeavesNoRowAndRetryKeepsOne(t *testing.T) {
	repo := &summaryStoreChunkRepo{chunks: map[string]*types.Chunk{}}
	svc := &knowledgeService{chunkRepo: repo}
	knowledge := &types.Knowledge{ID: "k1", TenantID: 1, KnowledgeBaseID: "kb"}
	var indexed, deletedFromIndex []string
	indexErr := errors.New("opensearch: transport error")

	err := svc.storeSummaryChunk(context.Background(), knowledge, "v1", 4, "t1",
		func(*types.Chunk) error { return indexErr },
		func(ids []string) error { deletedFromIndex = append(deletedFromIndex, ids...); return nil })
	require.ErrorIs(t, err, indexErr, "the indexing error must surface, not a cleanup result")
	assert.Empty(t, repo.summaries(), "no unindexed summary row may remain")
	assert.Len(t, deletedFromIndex, 1, "a possibly-written index entry is removed best effort")

	err = svc.storeSummaryChunk(context.Background(), knowledge, "v2", 4, "t1",
		func(c *types.Chunk) error { indexed = append(indexed, c.ID); return nil },
		func([]string) error { return nil })
	require.NoError(t, err)
	got := repo.summaries()
	require.Len(t, got, 1)
	assert.Equal(t, "v2", got[0].Content)
	assert.Equal(t, []string{got[0].ID}, indexed)
}

// A retry after an earlier attempt already stored a summary reuses it, and
// extra summary rows left by older runs are collapsed to one.
func TestStoreSummaryChunk_ReusesExistingAndDropsExtras(t *testing.T) {
	repo := &summaryStoreChunkRepo{chunks: map[string]*types.Chunk{
		"s1": {ID: "s1", KnowledgeID: "k1", ChunkType: types.ChunkTypeSummary, Content: "old"},
		"s2": {ID: "s2", KnowledgeID: "k1", ChunkType: types.ChunkTypeSummary, Content: "stale"},
		"t1": {ID: "t1", KnowledgeID: "k1", ChunkType: types.ChunkTypeText},
	}}
	svc := &knowledgeService{chunkRepo: repo}
	var deletedFromIndex []string
	err := svc.storeSummaryChunk(context.Background(), &types.Knowledge{ID: "k1", TenantID: 1}, "new", 4, "t1",
		func(*types.Chunk) error { return nil },
		func(ids []string) error { deletedFromIndex = append(deletedFromIndex, ids...); return nil })
	require.NoError(t, err)
	got := repo.summaries()
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].Content)
	assert.Len(t, deletedFromIndex, 1)
	assert.Contains(t, repo.chunks, "t1")

	// Indexing failure on a reused chunk keeps the row (it existed before).
	err = svc.storeSummaryChunk(context.Background(), &types.Knowledge{ID: "k1", TenantID: 1}, "newer", 4, "t1",
		func(*types.Chunk) error { return errors.New("down") },
		func([]string) error { t.Fatal("reused chunk must not be deleted"); return nil })
	require.Error(t, err)
	assert.Len(t, repo.summaries(), 1)
}
