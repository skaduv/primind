package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/primind/primind/internal/config"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
)

// qaShareService grants org-share read access to the KBs listed in shared.
type qaShareService struct {
	interfaces.KBShareService
	shared map[string]bool
}

func (s *qaShareService) CheckTenantKBPermission(
	_ context.Context, kbID string, _ uint64, _ types.TenantRole,
) (types.OrgMemberRole, bool, error) {
	if s.shared[kbID] {
		return types.OrgRoleViewer, true, nil
	}
	return "", false, nil
}

func newQAAccessSessionService(shares *qaShareService) *sessionService {
	return &sessionService{
		cfg: &config.Config{},
		knowledgeBaseService: &tagTargetKnowledgeBaseService{kbs: map[string]*types.KnowledgeBase{
			"kb-own":     {ID: "kb-own", TenantID: 200, Type: types.KnowledgeBaseTypeDocument},
			"kb-foreign": {ID: "kb-foreign", TenantID: 100, Type: types.KnowledgeBaseTypeDocument},
		}},
		knowledgeService: &tagTargetKnowledgeService{},
		kbShareService:   shares,
	}
}

func qaCallerContext() context.Context {
	return types.WithCaller(context.Background(), types.Caller{
		TenantID: 200, UserID: "user-b", Role: types.TenantRoleAdmin,
	})
}

// A QA request naming another tenant's KB must not carry that ID into the
// pipeline: graph search, the fallback document listing and model selection
// all read the resolved list, not just the permission-checked search targets.
func TestResolveKnowledgeBasesDropsKnowledgeBasesTheCallerCannotRead(t *testing.T) {
	shares := &qaShareService{shared: map[string]bool{}}
	svc := newQAAccessSessionService(shares)
	req := func() *types.QARequest {
		return &types.QARequest{
			Session:          &types.Session{ID: "s", TenantID: 200},
			KnowledgeBaseIDs: []string{"kb-foreign", "kb-own", "kb-missing"},
		}
	}

	kbIDs, _, err := svc.resolveKnowledgeBases(qaCallerContext(), req())
	require.NoError(t, err)
	assert.Equal(t, []string{"kb-own"}, kbIDs, "unshared foreign and unknown KBs are dropped")

	shares.shared["kb-foreign"] = true
	kbIDs, _, err = svc.resolveKnowledgeBases(qaCallerContext(), req())
	require.NoError(t, err)
	assert.Equal(t, []string{"kb-foreign", "kb-own"}, kbIDs, "an org share makes the KB readable")

	delete(shares.shared, "kb-foreign")
	kbIDs, _, err = svc.resolveKnowledgeBases(qaCallerContext(), req())
	require.NoError(t, err)
	assert.Equal(t, []string{"kb-own"}, kbIDs, "revoking the share removes it again")
}

func TestResolveKnowledgeBasesOnlyForeignKBsLeavesNoScope(t *testing.T) {
	svc := newQAAccessSessionService(&qaShareService{shared: map[string]bool{}})
	req := &types.QARequest{
		Session:          &types.Session{ID: "s", TenantID: 200},
		KnowledgeBaseIDs: []string{"kb-foreign"},
	}
	kbIDs, knowledgeIDs, err := svc.resolveKnowledgeBases(qaCallerContext(), req)
	require.NoError(t, err)
	assert.Empty(t, kbIDs)
	targets, err := svc.buildSearchTargets(qaCallerContext(), 200, kbIDs, knowledgeIDs, nil)
	require.NoError(t, err)
	assert.False(t, types.HasKnowledgeRetrievalScope(targets, kbIDs, knowledgeIDs),
		"the request falls through to the existing no-KB branch")
}

// ownerTenantSessionService knows the given KBs as owned by tenantID, for
// tests whose caller is that tenant (QA scope then keeps them).
func ownerTenantSessionService(tenantID uint64, kbIDs ...string) *sessionService {
	kbs := make(map[string]*types.KnowledgeBase, len(kbIDs))
	for _, id := range kbIDs {
		kbs[id] = &types.KnowledgeBase{ID: id, TenantID: tenantID, Type: types.KnowledgeBaseTypeDocument}
	}
	return &sessionService{cfg: &config.Config{}, knowledgeBaseService: &tagTargetKnowledgeBaseService{kbs: kbs}}
}

func ownerTenantContext(tenantID uint64) context.Context {
	return types.WithCaller(context.Background(), types.Caller{TenantID: tenantID, UserID: "owner"})
}
