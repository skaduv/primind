package service

import (
	"testing"

	"github.com/primind/primind/internal/types"
)

func TestResolveKnowledgeBasesFiltersImplicitAgentDefaultsForRestrictedAPIKey(t *testing.T) {
	ctx := types.WithTenantAPIKeyScope(ownerTenantContext(10000), types.TenantAPIKeyScope{
		KnowledgeBaseIDs: types.StringArray{"kb-allowed"},
	})
	svc := ownerTenantSessionService(10000, "kb-allowed", "kb-blocked")

	kbIDs, knowledgeIDs, err := svc.resolveKnowledgeBases(ctx, &types.QARequest{
		Session: &types.Session{TenantID: 10000},
		CustomAgent: &types.CustomAgent{
			TenantID: 10000,
			Config: types.CustomAgentConfig{
				KBSelectionMode: "selected",
				KnowledgeBases:  []string{"kb-allowed", "kb-blocked"},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveKnowledgeBases returned error: %v", err)
	}
	if len(kbIDs) != 1 || kbIDs[0] != "kb-allowed" {
		t.Fatalf("kbIDs = %#v, want only kb-allowed", kbIDs)
	}
	if len(knowledgeIDs) != 0 {
		t.Fatalf("knowledgeIDs = %#v, want empty", knowledgeIDs)
	}
}

func TestResolveKnowledgeBasesRejectsExplicitOutOfScopeKBForRestrictedAPIKey(t *testing.T) {
	ctx := types.WithTenantAPIKeyScope(ownerTenantContext(10000), types.TenantAPIKeyScope{
		KnowledgeBaseIDs: types.StringArray{"kb-allowed"},
	})
	svc := ownerTenantSessionService(10000, "kb-allowed", "kb-blocked")

	_, _, err := svc.resolveKnowledgeBases(ctx, &types.QARequest{
		Session:          &types.Session{TenantID: 10000},
		KnowledgeBaseIDs: []string{"kb-blocked"},
		CustomAgent: &types.CustomAgent{
			TenantID: 10000,
			Config: types.CustomAgentConfig{
				KBSelectionMode: "selected",
				KnowledgeBases:  []string{"kb-allowed", "kb-blocked"},
			},
		},
	})
	if err == nil {
		t.Fatal("expected forbidden for explicit out-of-scope knowledge_base_ids")
	}
}
