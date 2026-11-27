package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

const (
	resolverCallerTenant  = uint64(7)
	resolverSourceTenant  = uint64(84)
	resolverSharedAgentID = "shared-agent"
)

type resolverOwnAgents struct {
	interfaces.CustomAgentService
	agent *types.CustomAgent
	err   error
	calls int
}

func (s *resolverOwnAgents) GetAgentByID(context.Context, string) (*types.CustomAgent, error) {
	s.calls++
	return s.agent, s.err
}

type resolverShares struct {
	interfaces.AgentShareService
	agent  *types.CustomAgent
	err    error
	calls  int
	tenant uint64
	role   types.TenantRole
	source uint64
}

func (s *resolverShares) GetSharedAgentForTenant(
	_ context.Context,
	tenantID uint64,
	role types.TenantRole,
	_ string,
	source ...uint64,
) (*types.CustomAgent, error) {
	s.calls++
	s.tenant, s.role = tenantID, role
	if len(source) > 0 {
		s.source = source[0]
	}
	return s.agent, s.err
}

type resolverDisabledList struct {
	interfaces.TenantDisabledSharedAgentRepository
	records []*types.TenantDisabledSharedAgent
	err     error
	calls   int
	tenant  uint64
}

func (r *resolverDisabledList) ListByTenantID(
	_ context.Context,
	tenantID uint64,
) ([]*types.TenantDisabledSharedAgent, error) {
	r.calls++
	r.tenant = tenantID
	return r.records, r.err
}

type resolverKnowledge struct {
	interfaces.KnowledgeRepository
	byID  map[string]*types.Knowledge
	err   error
	calls int
	asked []string
}

func (r *resolverKnowledge) GetKnowledgeBatchByIDOnly(_ context.Context, ids []string) ([]*types.Knowledge, error) {
	r.calls++
	r.asked = append([]string{}, ids...)
	if r.err != nil {
		return nil, r.err
	}
	var rows []*types.Knowledge
	for _, id := range ids {
		if knowledge, ok := r.byID[id]; ok {
			rows = append(rows, knowledge)
		}
	}
	return rows, nil
}

// resolverKnowledgeBases answers which workspace owns each knowledge base.
type resolverKnowledgeBases struct {
	interfaces.KnowledgeBaseRepository
	owners map[string]uint64
	err    error
	calls  int
}

func (r *resolverKnowledgeBases) GetKnowledgeBaseByIDs(
	_ context.Context, ids []string,
) ([]*types.KnowledgeBase, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	var kbs []*types.KnowledgeBase
	for _, id := range ids {
		if owner, ok := r.owners[id]; ok {
			kbs = append(kbs, &types.KnowledgeBase{ID: id, TenantID: owner})
		}
	}
	return kbs, nil
}

func resolverSharedAgent() *types.CustomAgent {
	return &types.CustomAgent{
		ID:       resolverSharedAgentID,
		TenantID: resolverSourceTenant,
		Config: types.CustomAgentConfig{
			KBSelectionMode: "selected",
			KnowledgeBases:  []string{"kb-in"},
		},
	}
}

func resolverMember() AgentCaller {
	return AgentCaller{TenantID: resolverCallerTenant, UserID: "user-1", Role: types.TenantRoleAdmin}
}

// resolverLocalAgent is the caller's own agent, stored under the shared agent's ID.
func resolverLocalAgent() *types.CustomAgent {
	return &types.CustomAgent{ID: resolverSharedAgentID, TenantID: resolverCallerTenant}
}

// resolve and resolveForChat call the resolver the way a handler does, for the
// test's workspace member.
func resolve(resolver AgentAccessResolver, agentID string, source uint64) (*ResolvedAgent, error) {
	return resolver.Resolve(context.Background(), resolverMember(), agentID, source)
}

func resolveForChat(resolver AgentAccessResolver, agentID string, source uint64) (*ResolvedAgent, error) {
	return resolver.ResolveForChat(context.Background(), resolverMember(), agentID, source)
}

// The borrower's own agent wins without a source workspace, and no share is read.
func TestAgentAccessResolver_OwnAgentWithoutSource(t *testing.T) {
	own := &types.CustomAgent{ID: "own-agent", TenantID: resolverCallerTenant}
	agents := &resolverOwnAgents{agent: own}
	shares := &resolverShares{agent: resolverSharedAgent()}
	resolver := NewAgentAccessResolver(agents, shares, nil, nil, nil)

	resolved, err := resolver.Resolve(context.Background(), resolverMember(), own.ID, 0)

	require.NoError(t, err)
	require.Same(t, own, resolved.Agent)
	require.Equal(t, resolverCallerTenant, resolved.EffectiveTenantID)
	require.False(t, resolved.IsShared)
	require.True(t, resolved.KBScope.IsEmpty(), "an own agent's knowledge bases are not narrowed")
	require.Equal(t, 1, agents.calls)
	require.Zero(t, shares.calls)
}

// An old frontend names no source workspace; the agent is still found through
// the caller's share.
func TestAgentAccessResolver_ShareWithoutSource(t *testing.T) {
	shared := resolverSharedAgent()
	agents := &resolverOwnAgents{err: ErrAgentNotFound}
	shares := &resolverShares{agent: shared}
	resolver := NewAgentAccessResolver(agents, shares, nil, nil, nil)

	resolved, err := resolver.Resolve(context.Background(), resolverMember(), shared.ID, 0)

	require.NoError(t, err)
	require.Same(t, shared, resolved.Agent)
	require.True(t, resolved.IsShared)
	require.Equal(t, resolverSourceTenant, resolved.EffectiveTenantID)
	require.True(t, resolved.KBScope.Allows("kb-in", resolverSourceTenant))
	require.False(t, resolved.KBScope.Allows("kb-out", resolverSourceTenant))
	require.False(t, resolved.KBScope.Allows("kb-in", resolverCallerTenant))
	require.Equal(t, resolverCallerTenant, shares.tenant, "the share is looked up for the caller's workspace")
	require.Equal(t, types.TenantRoleAdmin, shares.role)
	require.Equal(t, uint64(0), shares.source)
}

// With a source workspace the ID is only a share from there: the caller's own
// agent of the same ID is not even read.
func TestAgentAccessResolver_SourceSelectorOnlyReadsShares(t *testing.T) {
	shared := resolverSharedAgent()
	agents := &resolverOwnAgents{agent: &types.CustomAgent{ID: shared.ID, TenantID: resolverCallerTenant}}
	shares := &resolverShares{agent: shared}
	resolver := NewAgentAccessResolver(agents, shares, nil, nil, nil)

	resolved, err := resolver.Resolve(context.Background(), resolverMember(), shared.ID, resolverSourceTenant)

	require.NoError(t, err)
	require.Same(t, shared, resolved.Agent)
	require.True(t, resolved.IsShared)
	require.Equal(t, resolverSourceTenant, shares.source)
	require.Zero(t, agents.calls)
}

// A share the caller cannot use is "not found" whether or not a source was
// named. With a source the caller's own agent of the same ID must not be used:
// the selector was explicit.
func TestAgentAccessResolver_RejectedShareNeverFallsBackToLocalAgent(t *testing.T) {
	for _, shareErr := range []error{ErrAgentShareNotFound, ErrAgentSharePermission, ErrAgentNotFoundForShare} {
		for _, tt := range []struct {
			name        string
			source      uint64
			own         *resolverOwnAgents
			wantOwnRead int
		}{
			{name: "no source, no local agent", own: &resolverOwnAgents{err: ErrAgentNotFound}, wantOwnRead: 1},
			{
				name:   "source selector shadows a local agent of the same ID",
				source: resolverSourceTenant,
				own:    &resolverOwnAgents{agent: resolverLocalAgent()},
			},
		} {
			t.Run(shareErr.Error()+"/"+tt.name, func(t *testing.T) {
				shares := &resolverShares{err: shareErr}
				resolver := NewAgentAccessResolver(tt.own, shares, nil, nil, nil)

				resolved, err := resolve(resolver, resolverSharedAgentID, tt.source)

				require.ErrorIs(t, err, ErrAgentNotFound)
				require.Nil(t, resolved)
				require.Equal(t, tt.wantOwnRead, tt.own.calls, "a refused selector never reads the local agent")
			})
		}
	}
}

// Built-in IDs exist in every workspace. Without a source they are the caller's
// own, and with one the share service refuses them outright.
func TestAgentAccessResolver_BuiltinIDStaysInCallerWorkspace(t *testing.T) {
	own := &types.CustomAgent{ID: types.BuiltinQuickAnswerID, TenantID: resolverCallerTenant, IsBuiltin: true}

	resolved, err := resolve(NewAgentAccessResolver(
		&resolverOwnAgents{agent: own},
		&resolverShares{agent: &types.CustomAgent{ID: own.ID, TenantID: resolverSourceTenant}},
		nil, nil, nil,
	), own.ID, 0)
	require.NoError(t, err)
	require.Same(t, own, resolved.Agent)
	require.False(t, resolved.IsShared)

	// With a source the share service refuses the built-in row itself, so a
	// share of one made before sharing them was refused cannot resolve either.
	resolved, err = resolve(NewAgentAccessResolver(
		&resolverOwnAgents{agent: own},
		&agentShareService{shareRepo: builtinShareRepo{}, agentRepo: &builtinShareAgentRepo{
			agent: &types.CustomAgent{ID: own.ID, TenantID: resolverSourceTenant, IsBuiltin: true},
		}},
		nil, nil, nil,
	), own.ID, resolverSourceTenant)
	require.ErrorIs(t, err, ErrAgentNotFound)
	require.Nil(t, resolved)
}

type resolverCountingShareRepo struct {
	interfaces.AgentShareRepository
	calls int
}

func (r *resolverCountingShareRepo) GetShareByAgentIDAndSourceForTenant(
	context.Context, uint64, string, uint64,
) (*types.AgentShare, error) {
	r.calls++
	return &types.AgentShare{SourceTenantID: resolverSourceTenant}, nil
}

func (r *resolverCountingShareRepo) GetShareByAgentIDForTenant(
	context.Context, uint64, string, uint64,
) (*types.AgentShare, error) {
	r.calls++
	return &types.AgentShare{SourceTenantID: resolverSourceTenant}, nil
}

// Naming the caller's own workspace as the source is not a share; the real
// share service refuses it before reading any share row.
func TestAgentAccessResolver_OwnWorkspaceAsSourceIsRefused(t *testing.T) {
	shareRepo := &resolverCountingShareRepo{}
	resolver := NewAgentAccessResolver(
		&resolverOwnAgents{agent: resolverLocalAgent()},
		&agentShareService{shareRepo: shareRepo, agentRepo: &builtinShareAgentRepo{agent: resolverSharedAgent()}},
		nil, nil, nil,
	)

	resolved, err := resolve(resolver, resolverSharedAgentID, resolverCallerTenant)

	require.ErrorIs(t, err, ErrAgentNotFound)
	require.Nil(t, resolved)
	require.Zero(t, shareRepo.calls)
}

// The workspace hid this share from its picker: every route but chat refuses it.
func TestAgentAccessResolver_RefusesShareDisabledInCallerWorkspace(t *testing.T) {
	disabled := &resolverDisabledList{records: []*types.TenantDisabledSharedAgent{
		{TenantID: resolverCallerTenant, AgentID: "another-agent", SourceTenantID: resolverSourceTenant},
		{TenantID: resolverCallerTenant, AgentID: resolverSharedAgentID, SourceTenantID: resolverSourceTenant},
	}}
	resolver := NewAgentAccessResolver(
		&resolverOwnAgents{err: ErrAgentNotFound}, &resolverShares{agent: resolverSharedAgent()}, disabled, nil, nil,
	)

	resolved, err := resolve(resolver, resolverSharedAgentID, resolverSourceTenant)

	require.ErrorIs(t, err, ErrAgentNotFound)
	require.Nil(t, resolved)
	require.Equal(t, resolverCallerTenant, disabled.tenant, "the list read belongs to the caller's workspace")

	// Chat predates the disabled list and is left unchanged here.
	chat, err := resolveForChat(resolver, resolverSharedAgentID, resolverSourceTenant)
	require.NoError(t, err)
	require.True(t, chat.IsShared)
}

// A record for another agent, another source, or one of the caller's own agents
// does not block this share.
func TestAgentAccessResolver_IgnoresUnrelatedDisabledRecords(t *testing.T) {
	shared := resolverSharedAgent()
	own := &types.CustomAgent{ID: "own-agent", TenantID: resolverCallerTenant}
	disabled := &resolverDisabledList{records: []*types.TenantDisabledSharedAgent{
		{TenantID: resolverCallerTenant, AgentID: shared.ID, SourceTenantID: resolverSourceTenant + 1},
		{TenantID: resolverCallerTenant, AgentID: own.ID, SourceTenantID: resolverCallerTenant},
	}}

	byShare, err := resolve(NewAgentAccessResolver(
		&resolverOwnAgents{err: ErrAgentNotFound}, &resolverShares{agent: shared}, disabled, nil, nil,
	), shared.ID, resolverSourceTenant)
	require.NoError(t, err)
	require.True(t, byShare.IsShared)

	byID, err := resolve(NewAgentAccessResolver(
		&resolverOwnAgents{agent: own}, &resolverShares{agent: shared}, disabled, nil, nil,
	), own.ID, 0)
	require.NoError(t, err)
	require.False(t, byID.IsShared, "the disabled list narrows shares only; own agents keep their path")
}

// A failing disabled list is an error, not "not found": the caller must not be
// told an agent is missing because a lookup broke.
func TestAgentAccessResolver_DisabledListFailureIsNotNotFound(t *testing.T) {
	disabled := &resolverDisabledList{err: errors.New("offline")}
	resolver := NewAgentAccessResolver(
		&resolverOwnAgents{err: ErrAgentNotFound}, &resolverShares{agent: resolverSharedAgent()}, disabled, nil, nil,
	)

	resolved, err := resolve(resolver, resolverSharedAgentID, 0)

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrAgentNotFound)
	require.Nil(t, resolved)
}

// Shares belong to a workspace member; without a user or a workspace there is
// nobody to look a share up for.
func TestAgentAccessResolver_ShareNeedsUserAndWorkspace(t *testing.T) {
	for name, caller := range map[string]AgentCaller{
		"no user":      {TenantID: resolverCallerTenant, Role: types.TenantRoleAdmin},
		"no workspace": {UserID: "user-1", Role: types.TenantRoleAdmin},
	} {
		t.Run(name, func(t *testing.T) {
			shares := &resolverShares{agent: resolverSharedAgent()}
			resolver := NewAgentAccessResolver(&resolverOwnAgents{err: ErrAgentNotFound}, shares, nil, nil, nil)

			resolved, err := resolver.Resolve(context.Background(), caller, resolverSharedAgentID, 0)

			require.ErrorIs(t, err, ErrAgentNotFound)
			require.Nil(t, resolved)
			require.Zero(t, shares.calls)
		})
	}
}

// An API key is a workspace member with a synthetic user. It resolves a share
// exactly as chat does for it; the key's own knowledge-base scope is applied
// downstream by the service, not here.
func TestAgentAccessResolver_APIKeyPrincipalResolvesSharesLikeChat(t *testing.T) {
	shared := resolverSharedAgent()
	shares := &resolverShares{agent: shared}
	resolver := NewAgentAccessResolver(&resolverOwnAgents{err: ErrAgentNotFound}, shares, nil, nil, nil)
	apiKey := AgentCaller{TenantID: resolverCallerTenant, UserID: "system-7", Role: types.TenantRoleViewer}

	resolved, err := resolver.Resolve(context.Background(), apiKey, shared.ID, resolverSourceTenant)

	require.NoError(t, err)
	require.Same(t, shared, resolved.Agent)
	require.Equal(t, types.TenantRoleViewer, shares.role)
}

// A broken store is not "agent not found".
func TestAgentAccessResolver_LookupFailureIsNotNotFound(t *testing.T) {
	for _, tt := range []struct {
		name     string
		shareErr error
		wired    bool
	}{
		{name: "share missed", shareErr: ErrAgentSharePermission, wired: true},
		{name: "share lookup failed too", shareErr: errors.New("share store down"), wired: true},
		{name: "no share service configured"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var shares interfaces.AgentShareService
			if tt.wired {
				shares = &resolverShares{err: tt.shareErr}
			}
			resolver := NewAgentAccessResolver(&resolverOwnAgents{err: errors.New("db down")}, shares, nil, nil, nil)

			resolved, err := resolver.Resolve(context.Background(), resolverMember(), resolverSharedAgentID, 0)

			require.Error(t, err)
			require.NotErrorIs(t, err, ErrAgentNotFound)
			require.Nil(t, resolved)
		})
	}
}

func TestAgentAccessResolver_EmptyIDIsNotFound(t *testing.T) {
	agents := &resolverOwnAgents{agent: &types.CustomAgent{ID: "own-agent"}}
	resolver := NewAgentAccessResolver(agents, &resolverShares{agent: resolverSharedAgent()}, nil, nil, nil)

	for _, resolve := range []func(context.Context, AgentCaller, string, uint64) (*ResolvedAgent, error){
		resolver.Resolve,
		resolver.ResolveForChat,
	} {
		resolved, err := resolve(context.Background(), resolverMember(), "", 0)
		require.ErrorIs(t, err, ErrAgentNotFound)
		require.Nil(t, resolved)
	}
	require.Zero(t, agents.calls)
}

func TestAgentAccessResolver_RestrictSuggestionScope(t *testing.T) {
	own := &ResolvedAgent{
		Agent:             &types.CustomAgent{ID: "own-agent", TenantID: resolverCallerTenant},
		EffectiveTenantID: resolverCallerTenant,
	}
	allKBs := resolverSharedAgent()
	allKBs.Config = types.CustomAgentConfig{KBSelectionMode: "all"}
	shared := func(agent *types.CustomAgent) *ResolvedAgent {
		return &ResolvedAgent{
			Agent:             agent,
			EffectiveTenantID: agent.TenantID,
			IsShared:          true,
			KBScope:           types.NewSharedAgentKBScope(agent),
		}
	}
	selected := shared(resolverSharedAgent())
	documents := map[string]*types.Knowledge{
		"doc-in":      {ID: "doc-in", TenantID: resolverSourceTenant, KnowledgeBaseID: "kb-in"},
		"doc-out":     {ID: "doc-out", TenantID: resolverSourceTenant, KnowledgeBaseID: "kb-out"},
		"doc-own":     {ID: "doc-own", TenantID: resolverCallerTenant, KnowledgeBaseID: "kb-in"},
		"doc-foreign": {ID: "doc-foreign", TenantID: resolverSourceTenant + 1, KnowledgeBaseID: "kb-in"},
	}
	// kb-own is the caller's own knowledge base and kb-foreign belongs to a
	// third workspace; kb-missing does not exist.
	kbOwners := map[string]uint64{
		"kb-in":      resolverSourceTenant,
		"kb-out":     resolverSourceTenant,
		"kb-own":     resolverCallerTenant,
		"kb-foreign": resolverSourceTenant + 1,
	}
	full := SuggestionScope{
		KnowledgeBaseIDs: []string{"kb-in", "kb-out"},
		KnowledgeIDs:     []string{"doc-in", "doc-out", "doc-own", "doc-missing", "doc-foreign"},
		TagScopes: []types.TagScope{
			{KnowledgeBaseID: "kb-in", TagIDs: []string{"tag-in"}},
			{KnowledgeBaseID: "kb-out", TagIDs: []string{"tag-out"}},
		},
	}
	outsideSource := SuggestionScope{
		KnowledgeBaseIDs: []string{"kb-own", "kb-foreign", "kb-missing"},
		TagScopes: []types.TagScope{
			{KnowledgeBaseID: "kb-own", TagIDs: []string{"tag-own"}},
			{KnowledgeBaseID: "kb-foreign", TagIDs: []string{"tag-foreign"}},
		},
	}

	for _, tt := range []struct {
		name        string
		agent       *ResolvedAgent
		scope       SuggestionScope
		storeErr    error
		kbStoreErr  error
		noStore     bool
		noKBStore   bool
		want        SuggestionScope
		empty       bool
		wantReads   int
		wantKBReads int
	}{
		{
			name:  "own agent keeps its request untouched",
			agent: own,
			scope: full,
			want:  full,
		},
		{
			name:  "selected share drops everything outside its knowledge bases",
			agent: selected,
			scope: full,
			want: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-in"},
				KnowledgeIDs:     []string{"doc-in"},
				TagScopes:        []types.TagScope{{KnowledgeBaseID: "kb-in", TagIDs: []string{"tag-in"}}},
			},
			wantReads:   1,
			wantKBReads: 1,
		},
		{
			name:  "whole-workspace share admits its own workspace's knowledge bases only",
			agent: shared(allKBs),
			scope: full,
			want: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-in", "kb-out"},
				KnowledgeIDs:     []string{"doc-in", "doc-out"},
				TagScopes:        full.TagScopes,
			},
			wantReads:   1,
			wantKBReads: 1,
		},
		{
			name:        "whole-workspace share refuses own, third-workspace and unknown knowledge bases",
			agent:       shared(allKBs),
			scope:       outsideSource,
			want:        SuggestionScope{},
			empty:       true,
			wantKBReads: 1,
		},
		{
			name:  "whole-workspace share keeps its own knowledge bases among outside ones",
			agent: shared(allKBs),
			scope: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-own", "kb-in"},
				TagScopes:        []types.TagScope{{KnowledgeBaseID: "kb-own", TagIDs: []string{"tag-own"}}},
			},
			want:        SuggestionScope{KnowledgeBaseIDs: []string{"kb-in"}},
			wantKBReads: 1,
		},
		{
			name:  "selected share with no range named falls back to the agent configuration",
			agent: selected,
			want:  SuggestionScope{},
		},
		{
			name:        "range entirely outside the share is empty, not the agent's own range",
			agent:       selected,
			scope:       SuggestionScope{KnowledgeBaseIDs: []string{"kb-out"}},
			want:        SuggestionScope{},
			empty:       true,
			wantKBReads: 1,
		},
		{
			name:      "documents entirely outside the share are empty too",
			agent:     selected,
			scope:     SuggestionScope{KnowledgeIDs: []string{"doc-out", "doc-own", "doc-missing"}},
			want:      SuggestionScope{},
			empty:     true,
			wantReads: 1,
		},
		{
			name:  "tag scope entirely outside the share is empty too",
			agent: selected,
			scope: SuggestionScope{TagScopes: []types.TagScope{
				{KnowledgeBaseID: "kb-out", TagIDs: []string{"tag-out"}},
			}},
			want:        SuggestionScope{},
			empty:       true,
			wantKBReads: 1,
		},
		{
			name:  "unresolvable documents are dropped without failing the request",
			agent: selected,
			scope: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-in"},
				KnowledgeIDs:     []string{"doc-in", "doc-out"},
			},
			storeErr:    errors.New("offline"),
			want:        SuggestionScope{KnowledgeBaseIDs: []string{"kb-in"}},
			wantReads:   1,
			wantKBReads: 1,
		},
		{
			name:  "unresolvable knowledge bases are dropped without failing the request",
			agent: shared(allKBs),
			scope: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-in"},
				KnowledgeIDs:     []string{"doc-in"},
				TagScopes:        []types.TagScope{{KnowledgeBaseID: "kb-in", TagIDs: []string{"tag-in"}}},
			},
			kbStoreErr:  errors.New("offline"),
			want:        SuggestionScope{KnowledgeIDs: []string{"doc-in"}},
			wantReads:   1,
			wantKBReads: 1,
		},
		{
			name:  "no document store drops documents and keeps the rest",
			agent: selected,
			scope: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-in"},
				KnowledgeIDs:     []string{"doc-in"},
			},
			noStore:     true,
			want:        SuggestionScope{KnowledgeBaseIDs: []string{"kb-in"}},
			wantKBReads: 1,
		},
		{
			name:  "no knowledge base store drops knowledge bases and tag scopes",
			agent: shared(allKBs),
			scope: SuggestionScope{
				KnowledgeBaseIDs: []string{"kb-in"},
				TagScopes:        []types.TagScope{{KnowledgeBaseID: "kb-in", TagIDs: []string{"tag-in"}}},
			},
			noKBStore: true,
			want:      SuggestionScope{},
			empty:     true,
		},
		{
			name:  "nil resolution passes the request through",
			agent: nil,
			scope: full,
			want:  full,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var knowledge interfaces.KnowledgeRepository
			store := &resolverKnowledge{byID: documents, err: tt.storeErr}
			if !tt.noStore {
				knowledge = store
			}
			var knowledgeBases interfaces.KnowledgeBaseRepository
			kbStore := &resolverKnowledgeBases{owners: kbOwners, err: tt.kbStoreErr}
			if !tt.noKBStore {
				knowledgeBases = kbStore
			}
			resolver := NewAgentAccessResolver(nil, nil, nil, knowledge, knowledgeBases)

			got, err := resolver.RestrictSuggestionScope(context.Background(), tt.agent, tt.scope)

			require.NoError(t, err)
			require.Equal(t, tt.want, got.Scope)
			require.Equal(t, tt.empty, got.EmptyByScope)
			require.Equal(t, tt.wantReads, store.calls, "document lookups")
			require.Equal(t, tt.wantKBReads, kbStore.calls, "knowledge base lookups")
		})
	}
}

// However many document IDs a request names, the share's range is checked
// with one lookup of at most maxSuggestionKnowledgeIDs distinct IDs.
func TestAgentAccessResolver_RestrictSuggestionScopeBoundsDocumentLookups(t *testing.T) {
	agent := resolverSharedAgent()
	resolved := &ResolvedAgent{
		Agent: agent, EffectiveTenantID: agent.TenantID, IsShared: true,
		KBScope: types.NewSharedAgentKBScope(agent),
	}
	documents := map[string]*types.Knowledge{}
	var ids []string
	for i := 0; i < 3*maxSuggestionKnowledgeIDs; i++ {
		id := fmt.Sprintf("doc-%03d", i)
		documents[id] = &types.Knowledge{ID: id, TenantID: resolverSourceTenant, KnowledgeBaseID: "kb-in"}
		// Every ID twice, and empty entries in between.
		ids = append(ids, id, "", id)
	}
	store := &resolverKnowledge{byID: documents}
	resolver := NewAgentAccessResolver(nil, nil, nil, store, nil)

	got, err := resolver.RestrictSuggestionScope(context.Background(), resolved, SuggestionScope{KnowledgeIDs: ids})

	require.NoError(t, err)
	require.Equal(t, 1, store.calls)
	require.Len(t, store.asked, maxSuggestionKnowledgeIDs)
	require.Equal(t, "doc-000", store.asked[0])
	require.Equal(t, fmt.Sprintf("doc-%03d", maxSuggestionKnowledgeIDs-1), store.asked[maxSuggestionKnowledgeIDs-1])
	require.Equal(t, store.asked, got.Scope.KnowledgeIDs)
}
