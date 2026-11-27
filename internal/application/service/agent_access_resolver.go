package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	secutils "github.com/primind/primind/internal/utils"
)

// AgentCaller is the identity asking to use an agent: the workspace the request
// runs in, the user, and that user's role in the workspace. An API key carries
// its workspace's (possibly synthetic) user, exactly as it always has on the
// chat path.
type AgentCaller struct {
	TenantID uint64
	UserID   string
	Role     types.TenantRole
}

// ResolvedAgent is an agent the caller may use.
type ResolvedAgent struct {
	Agent *types.CustomAgent
	// EffectiveTenantID is the workspace the agent lives in: the caller's own
	// for an own agent, the lending one for a share.
	EffectiveTenantID uint64
	IsShared          bool
	// KBScope is what a shared agent lets its borrower read. It is the zero
	// value, which authorizes nothing, for an own agent: its knowledge bases
	// are the caller's own and are not narrowed here.
	KBScope types.SharedAgentKBScope
}

// SuggestionScope is the explicit knowledge range of a suggested-questions
// request. Empty fields mean "unspecified", never "nothing".
type SuggestionScope struct {
	KnowledgeBaseIDs []string
	KnowledgeIDs     []string
	TagScopes        []types.TagScope
}

// RestrictedSuggestionScope is a SuggestionScope narrowed to one agent's grant.
type RestrictedSuggestionScope struct {
	Scope SuggestionScope
	// EmptyByScope reports that the request named a range and none of it lies
	// inside the shared agent's. The caller must answer with an empty list:
	// passing the emptied fields on would read as "unspecified" and fall back
	// to every knowledge base the agent can see.
	EmptyByScope bool
}

// AgentAccessResolver is the single place that decides whether a caller may use
// an agent addressed by ID. Every route that reads an agent by ID resolves it
// here and is listed in internal/router/agent_routes_contract_test.go.
type AgentAccessResolver interface {
	// Resolve returns the agent when the caller may use it now.
	//
	// Without a source workspace the ID names the caller's own agent first and
	// a share second. With one, it is only a share from that workspace: a
	// rejected selector never falls back to a same-ID local agent. Built-in IDs
	// resolve to the caller's own workspace only, and a share the caller's
	// workspace has disabled is refused.
	//
	// An unknown agent, one without a share, and a revoked share all return
	// ErrAgentNotFound, so the answer reveals nothing about other workspaces.
	// Any other error is a failed lookup and must not be reported as "not found".
	Resolve(ctx context.Context, caller AgentCaller, agentID string, sourceTenantID uint64) (*ResolvedAgent, error)

	// ResolveForChat is Resolve without the "disabled in this workspace"
	// refusal. The chat path predates that list and still runs a share that a
	// restored selection names; hiding it from the picker was the list's only
	// contract. The gap is recorded in
	// docs/bug/2026-11-26-本空间停用的共享智能体仍可在对话中使用.md and stays
	// open until that is fixed, so chat behaviour does not change here.
	ResolveForChat(
		ctx context.Context, caller AgentCaller, agentID string, sourceTenantID uint64,
	) (*ResolvedAgent, error)

	// RestrictSuggestionScope narrows scope to what agent lets the caller read.
	RestrictSuggestionScope(
		ctx context.Context, agent *ResolvedAgent, scope SuggestionScope,
	) (RestrictedSuggestionScope, error)
}

// knowledgeOwnerLookup reports which knowledge base and workspace own each
// document, in one query. interfaces.KnowledgeRepository satisfies it.
type knowledgeOwnerLookup interface {
	GetKnowledgeBatchByIDOnly(ctx context.Context, ids []string) ([]*types.Knowledge, error)
}

// knowledgeBaseOwnerLookup reports which workspace owns each knowledge base, in
// one query. interfaces.KnowledgeBaseRepository satisfies it.
type knowledgeBaseOwnerLookup interface {
	GetKnowledgeBaseByIDs(ctx context.Context, ids []string) ([]*types.KnowledgeBase, error)
}

type agentAccessResolver struct {
	agents         interfaces.CustomAgentService
	shares         interfaces.AgentShareService
	disabled       interfaces.TenantDisabledSharedAgentRepository
	knowledges     knowledgeOwnerLookup
	knowledgeBases knowledgeBaseOwnerLookup
}

// NewAgentAccessResolver builds the resolver from the services it reads. A nil
// share service or disabled list only removes the check that needs it; own
// agents still resolve. A nil knowledge or knowledge-base repository means a
// shared agent's request range cannot be checked, so that part of it is dropped.
func NewAgentAccessResolver(
	agents interfaces.CustomAgentService,
	shares interfaces.AgentShareService,
	disabledRepo interfaces.TenantDisabledSharedAgentRepository,
	knowledgeRepo interfaces.KnowledgeRepository,
	knowledgeBaseRepo interfaces.KnowledgeBaseRepository,
) AgentAccessResolver {
	return &agentAccessResolver{
		agents:         agents,
		shares:         shares,
		disabled:       disabledRepo,
		knowledges:     knowledgeRepo,
		knowledgeBases: knowledgeBaseRepo,
	}
}

func (r *agentAccessResolver) ResolveForChat(
	ctx context.Context,
	caller AgentCaller,
	agentID string,
	sourceTenantID uint64,
) (*ResolvedAgent, error) {
	if agentID == "" {
		return nil, ErrAgentNotFound
	}

	logger.Infof(ctx, "Resolving agent, agent ID: %s", secutils.SanitizeForLog(agentID))

	// Without a source workspace the ID names the caller's own agent first.
	// Built-in IDs exist in every workspace, so trying shares first would let
	// any org member that shares an agent under such an ID take over the
	// caller's default agent (and run the caller's chats in its workspace).
	// A rejected shared selector (sourceTenantID != 0) must likewise never
	// fall back to a same-ID local agent.
	var ownErr error
	if sourceTenantID == 0 {
		agent, err := r.agents.GetAgentByID(ctx, agentID)
		if err == nil && agent != nil {
			logger.Infof(ctx, "Using own agent: ID=%s, Name=%s, AgentMode=%s",
				agent.ID, agent.Name, agent.Config.AgentMode)
			return &ResolvedAgent{Agent: agent, EffectiveTenantID: caller.TenantID}, nil
		}
		ownErr = err
	}

	// A share is looked up for the caller's workspace, so it takes a user in one.
	var shareErr error
	if r.shares != nil && caller.UserID != "" && caller.TenantID != 0 {
		agent, err := r.shares.GetSharedAgentForTenant(ctx, caller.TenantID, caller.Role, agentID, sourceTenantID)
		if err == nil && agent != nil {
			logger.Infof(ctx, "Using shared agent: ID=%s, Name=%s, IsBuiltin=%v, AgentMode=%s, effectiveTenantID=%d",
				agent.ID, agent.Name, agent.IsBuiltin, agent.Config.AgentMode, agent.TenantID)
			return &ResolvedAgent{
				Agent:             agent,
				EffectiveTenantID: agent.TenantID,
				IsShared:          true,
				KBScope:           types.NewSharedAgentKBScope(agent),
			}, nil
		}
		shareErr = err
	}

	logger.Warnf(ctx, "Failed to get agent, agent ID: %s, source tenant: %d, own error: %v, share error: %v",
		secutils.SanitizeForLog(agentID), sourceTenantID, ownErr, shareErr)
	if !agentLookupMissed(ownErr) || !agentLookupMissed(shareErr) {
		return nil, fmt.Errorf("agent access resolver: own lookup: %v, share lookup: %w", ownErr, shareErr)
	}
	return nil, ErrAgentNotFound
}

func (r *agentAccessResolver) Resolve(
	ctx context.Context,
	caller AgentCaller,
	agentID string,
	sourceTenantID uint64,
) (*ResolvedAgent, error) {
	resolved, err := r.ResolveForChat(ctx, caller, agentID, sourceTenantID)
	if err != nil || !resolved.IsShared || r.disabled == nil {
		return resolved, err
	}

	disabled, err := r.disabled.ListByTenantID(ctx, caller.TenantID)
	if err != nil {
		return nil, fmt.Errorf("agent access resolver: listing disabled shared agents: %w", err)
	}
	for _, record := range disabled {
		if record != nil && record.AgentID == resolved.Agent.ID && record.SourceTenantID == resolved.EffectiveTenantID {
			logger.Infof(ctx, "Refusing shared agent disabled in the caller's workspace: ID=%s, effectiveTenantID=%d",
				resolved.Agent.ID, resolved.EffectiveTenantID)
			return nil, ErrAgentNotFound
		}
	}
	return resolved, nil
}

func (r *agentAccessResolver) RestrictSuggestionScope(
	ctx context.Context,
	agent *ResolvedAgent,
	scope SuggestionScope,
) (RestrictedSuggestionScope, error) {
	if agent == nil || !agent.IsShared {
		return RestrictedSuggestionScope{Scope: scope}, nil
	}

	named := len(scope.KnowledgeBaseIDs) > 0 || len(scope.KnowledgeIDs) > 0 || len(scope.TagScopes) > 0

	// A knowledge base is inside the share only when it lives in the lending
	// workspace and the share selects it, so each one is checked against the
	// workspace that actually owns it: a whole-workspace share must not admit
	// the caller's own or another workspace's knowledge base by its ID alone.
	// Knowledge bases named directly and by tag scopes resolve in one query.
	kbIDs := make([]string, 0, len(scope.KnowledgeBaseIDs)+len(scope.TagScopes))
	kbIDs = append(kbIDs, scope.KnowledgeBaseIDs...)
	for _, tagScope := range scope.TagScopes {
		kbIDs = append(kbIDs, tagScope.KnowledgeBaseID)
	}
	kbOwners := r.knowledgeBaseOwners(ctx, kbIDs)
	allowsKB := func(kbID string) bool {
		owner, ok := kbOwners[kbID]
		return ok && agent.KBScope.Allows(kbID, owner)
	}

	restricted := SuggestionScope{}
	for _, kbID := range scope.KnowledgeBaseIDs {
		if allowsKB(kbID) {
			restricted.KnowledgeBaseIDs = append(restricted.KnowledgeBaseIDs, kbID)
		}
	}
	for _, tagScope := range scope.TagScopes {
		if allowsKB(tagScope.KnowledgeBaseID) {
			restricted.TagScopes = append(restricted.TagScopes, tagScope)
		}
	}
	// A document is only inside the scope when its own knowledge base is.
	for _, knowledge := range r.knowledgeOwners(ctx, scope.KnowledgeIDs) {
		if agent.KBScope.Allows(knowledge.KnowledgeBaseID, knowledge.TenantID) {
			restricted.KnowledgeIDs = append(restricted.KnowledgeIDs, knowledge.ID)
		}
	}

	kept := len(restricted.KnowledgeBaseIDs) + len(restricted.KnowledgeIDs) + len(restricted.TagScopes)
	logger.Infof(ctx,
		"Restricted suggestion scope for shared agent %s: knowledge bases %d/%d, documents %d/%d, tag scopes %d/%d",
		resolvedAgentID(agent),
		len(restricted.KnowledgeBaseIDs), len(scope.KnowledgeBaseIDs),
		len(restricted.KnowledgeIDs), len(scope.KnowledgeIDs),
		len(restricted.TagScopes), len(scope.TagScopes))
	return RestrictedSuggestionScope{
		Scope:        restricted,
		EmptyByScope: named && kept == 0,
	}, nil
}

// knowledgeBaseOwners maps each resolvable knowledge base ID to the workspace
// that owns it. A missing row, a failing store and an unconfigured one all
// leave the ID out, so the caller drops it: the check fails closed.
func (r *agentAccessResolver) knowledgeBaseOwners(ctx context.Context, kbIDs []string) map[string]uint64 {
	kbIDs = uniqueNonEmpty(kbIDs, 0)
	owners := make(map[string]uint64, len(kbIDs))
	if len(kbIDs) == 0 {
		return owners
	}
	if r.knowledgeBases == nil {
		logger.Warnf(ctx, "Dropping %d knowledge bases from a shared agent's suggestion scope: "+
			"knowledge base lookup is not configured", len(kbIDs))
		return owners
	}
	kbs, err := r.knowledgeBases.GetKnowledgeBaseByIDs(ctx, kbIDs)
	if err != nil {
		logger.Warnf(ctx, "Dropping %d knowledge bases from a shared agent's suggestion scope: %v", len(kbIDs), err)
		return owners
	}
	for _, kb := range kbs {
		if kb != nil && kb.ID != "" && kb.TenantID != 0 {
			owners[kb.ID] = kb.TenantID
		}
	}
	return owners
}

// knowledgeOwners resolves the documents a request names, in request order.
// IDs are deduplicated and capped at maxSuggestionKnowledgeIDs, as the
// suggestion service caps them, so one request cannot fan out into an
// unbounded lookup. Unresolvable documents are left out (fail closed).
func (r *agentAccessResolver) knowledgeOwners(ctx context.Context, knowledgeIDs []string) []*types.Knowledge {
	knowledgeIDs = uniqueNonEmpty(knowledgeIDs, maxSuggestionKnowledgeIDs)
	if len(knowledgeIDs) == 0 {
		return nil
	}
	if r.knowledges == nil {
		logger.Warnf(ctx, "Dropping %d documents from a shared agent's suggestion scope: "+
			"knowledge lookup is not configured", len(knowledgeIDs))
		return nil
	}
	rows, err := r.knowledges.GetKnowledgeBatchByIDOnly(ctx, knowledgeIDs)
	if err != nil {
		logger.Warnf(ctx, "Dropping %d documents from a shared agent's suggestion scope: %v", len(knowledgeIDs), err)
		return nil
	}
	byID := make(map[string]*types.Knowledge, len(rows))
	for _, row := range rows {
		if row != nil {
			byID[row.ID] = row
		}
	}
	owners := make([]*types.Knowledge, 0, len(knowledgeIDs))
	for _, id := range knowledgeIDs {
		if knowledge, ok := byID[id]; ok {
			owners = append(owners, knowledge)
		}
	}
	return owners
}

// uniqueNonEmpty drops empty and repeated IDs, keeping the first occurrence,
// and keeps at most limit of them when limit is positive.
func uniqueNonEmpty(ids []string, limit int) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// agentLookupMissed reports whether err means "no such agent or share" rather
// than a failed lookup. nil counts: the lookup then found no share to offer.
func agentLookupMissed(err error) bool {
	return err == nil ||
		errors.Is(err, ErrAgentNotFound) ||
		errors.Is(err, ErrAgentShareNotFound) ||
		errors.Is(err, ErrAgentSharePermission) ||
		errors.Is(err, ErrAgentNotFoundForShare)
}

func resolvedAgentID(agent *ResolvedAgent) string {
	if agent == nil || agent.Agent == nil {
		return ""
	}
	return secutils.SanitizeForLog(agent.Agent.ID)
}
