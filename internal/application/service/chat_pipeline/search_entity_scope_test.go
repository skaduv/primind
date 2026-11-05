package chatpipeline

import (
	"context"
	"testing"

	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

type entityGraphRepo struct {
	interfaces.RetrieveGraphRepository
	graph *types.GraphData
}

func (r *entityGraphRepo) SearchNode(context.Context, types.NameSpace, []string) (*types.GraphData, error) {
	return r.graph, nil
}

type entityKnowledgeRepo struct {
	interfaces.KnowledgeRepository
	knowledge map[string]*types.Knowledge
}

func (r *entityKnowledgeRepo) GetKnowledgeBatchByIDOnly(_ context.Context, ids []string) ([]*types.Knowledge, error) {
	var out []*types.Knowledge
	for _, id := range ids {
		if k := r.knowledge[id]; k != nil {
			out = append(out, k)
		}
	}
	return out, nil
}

// Graph hits resolve across workspaces for the KBs in scope, never outside
// them, and a chunk whose document is gone is skipped instead of panicking.
func TestSearchEntityResolvesSharedChunksWithinScope(t *testing.T) {
	plugin := &PluginSearchEntity{
		graphRepo: &entityGraphRepo{graph: &types.GraphData{Node: []*types.GraphNode{
			{Name: "Docker", Chunks: []string{"shared", "outside", "orphan"}},
		}}},
		chunkRepo: &expandChunkRepo{chunks: map[string]*types.Chunk{
			"shared":  entityChunk("shared", 2, "kb-shared", "doc"),
			"outside": entityChunk("outside", 3, "kb-other", "doc-x"),
			"orphan":  entityChunk("orphan", 2, "kb-shared", "gone"),
		}},
		knowledgeRepo: &entityKnowledgeRepo{knowledge: map[string]*types.Knowledge{
			"doc":   {ID: "doc", KnowledgeBaseID: "kb-shared", Title: "Shared doc"},
			"doc-x": {ID: "doc-x", KnowledgeBaseID: "kb-other", Title: "Other"},
		}},
	}
	cm := &types.ChatManage{PipelineState: types.PipelineState{
		Entity: []string{"Docker"}, EntityKBIDs: []string{"kb-shared"},
	}}
	cm.SearchTargets = types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-shared", TenantID: 2},
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(1))

	if err := plugin.OnEvent(ctx, types.ENTITY_SEARCH, cm, func() *PluginError { return nil }); err != nil {
		t.Fatalf("OnEvent: %v", err)
	}
	if len(cm.SearchResult) != 1 || cm.SearchResult[0].ID != "shared" ||
		cm.SearchResult[0].KnowledgeTitle != "Shared doc" {
		t.Fatalf("entity results = %+v", cm.SearchResult)
	}
}

func entityChunk(id string, tenantID uint64, kbID, knowledgeID string) *types.Chunk {
	return &types.Chunk{
		ID: id, TenantID: tenantID, KnowledgeBaseID: kbID, KnowledgeID: knowledgeID, IsEnabled: true, Content: id,
	}
}

// Graph namespaces are only searched for KBs the turn is authorized for: a KB
// named in the request but absent from the permission-checked search targets
// (another tenant's, unshared) must not contribute chunks or references.
func TestSearchEntityIgnoresKnowledgeBasesOutsideSearchTargets(t *testing.T) {
	plugin := &PluginSearchEntity{
		graphRepo: &entityGraphRepo{graph: &types.GraphData{Node: []*types.GraphNode{
			{Name: "Docker", Chunks: []string{"foreign", "own"}},
		}}},
		chunkRepo: &expandChunkRepo{chunks: map[string]*types.Chunk{
			"foreign": entityChunk("foreign", 100, "kb-foreign", "doc-f"),
			"own":     entityChunk("own", 200, "kb-own", "doc-o"),
		}},
		knowledgeRepo: &entityKnowledgeRepo{knowledge: map[string]*types.Knowledge{
			"doc-f": {ID: "doc-f", KnowledgeBaseID: "kb-foreign", Title: "Foreign"},
			"doc-o": {ID: "doc-o", KnowledgeBaseID: "kb-own", Title: "Own"},
		}},
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(200))
	for _, tc := range []struct {
		name    string
		state   types.PipelineState
		targets types.SearchTargets
		want    []string
	}{
		{
			"kb scope",
			types.PipelineState{Entity: []string{"Docker"}, EntityKBIDs: []string{"kb-foreign", "kb-own"}},
			types.SearchTargets{{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-own", TenantID: 200}},
			[]string{"own"},
		},
		{
			"file scope",
			types.PipelineState{
				Entity:          []string{"Docker"},
				EntityKnowledge: map[string]string{"doc-f": "kb-foreign", "doc-o": "kb-own"},
			},
			types.SearchTargets{{
				Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb-own", TenantID: 200,
				KnowledgeIDs: []string{"doc-o"},
			}},
			[]string{"own"},
		},
		{
			"no authorized target",
			types.PipelineState{Entity: []string{"Docker"}, EntityKBIDs: []string{"kb-foreign"}},
			nil, nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm := &types.ChatManage{PipelineState: tc.state}
			cm.SearchTargets = tc.targets
			if err := plugin.OnEvent(ctx, types.ENTITY_SEARCH, cm, func() *PluginError { return nil }); err != nil {
				t.Fatalf("OnEvent: %v", err)
			}
			var got []string
			for _, r := range cm.SearchResult {
				got = append(got, r.ID)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
				t.Fatalf("results = %v, want %v", got, tc.want)
			}
		})
	}
}
