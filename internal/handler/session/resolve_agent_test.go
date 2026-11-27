package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/application/service"
	"github.com/primind/primind/internal/types"
	"github.com/stretchr/testify/require"
)

type resolveAgentShareStub struct {
	agent *types.CustomAgent
	err   error
}

func (s *resolveAgentShareStub) GetSharedAgentForTenant(
	_ context.Context,
	_ uint64,
	_ types.TenantRole,
	_ string,
	_ ...uint64,
) (*types.CustomAgent, error) {
	return s.agent, s.err
}

func (s *resolveAgentShareStub) ShareAgent(context.Context, string, string, string, uint64, types.OrgMemberRole) (*types.AgentShare, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) RemoveShare(context.Context, string, string, uint64) error {
	panic("not implemented")
}
func (s *resolveAgentShareStub) ListSharesByAgent(context.Context, string, uint64) ([]*types.AgentShare, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) ListSharesByOrganization(context.Context, string) ([]*types.AgentShare, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) ListSharedAgents(context.Context, uint64, types.TenantRole) ([]*types.SharedAgentInfo, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) ListSharedAgentsInOrganization(context.Context, string, uint64, types.TenantRole) ([]*types.OrganizationSharedAgentItem, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) ListSharedAgentsInOrganizations(context.Context, []string, uint64, types.TenantRole) (map[string][]*types.OrganizationSharedAgentItem, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) SetSharedAgentDisabledByMe(context.Context, uint64, string, uint64, bool) error {
	panic("not implemented")
}
func (s *resolveAgentShareStub) TenantCanAccessKBViaSomeSharedAgent(context.Context, uint64, types.TenantRole, *types.KnowledgeBase) (bool, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) GetShare(context.Context, string) (*types.AgentShare, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) GetShareByAgentAndOrg(context.Context, string, string) (*types.AgentShare, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) GetShareByAgentIDForTenant(context.Context, uint64, string, uint64) (*types.AgentShare, error) {
	panic("not implemented")
}
func (s *resolveAgentShareStub) CountByOrganizations(context.Context, []string) (map[string]int64, error) {
	panic("not implemented")
}

type resolveOwnAgentStub struct {
	agent *types.CustomAgent
	err   error
}

func (s *resolveOwnAgentStub) GetAgentByID(context.Context, string) (*types.CustomAgent, error) {
	return s.agent, s.err
}

func (s *resolveOwnAgentStub) CreateAgent(context.Context, *types.CustomAgent) (*types.CustomAgent, error) {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) GetAgentByIDAndTenant(context.Context, string, uint64) (*types.CustomAgent, error) {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) ListAgents(context.Context) ([]*types.CustomAgent, error) {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) UpdateAgent(
	context.Context, *types.CustomAgent, *string,
) (*types.CustomAgent, error) {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) DeleteAgent(context.Context, string) error {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) CopyAgent(context.Context, string) (*types.CustomAgent, error) {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) GetSuggestedQuestions(context.Context, string, []string, []string, []types.TagScope, int) ([]types.SuggestedQuestion, error) {
	panic("not implemented")
}
func (s *resolveOwnAgentStub) GetKnowledgeSuggestedQuestions(context.Context, string, []string, []string, []types.TagScope, int) ([]types.SuggestedQuestion, error) {
	panic("not implemented")
}

func newResolveAgentTestContext(tenantID uint64) (*gin.Context, context.Context) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set(types.UserIDContextKey.String(), "user-1")
	c.Set(types.TenantIDContextKey.String(), tenantID)
	ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, tenantID)
	c.Request = c.Request.WithContext(ctx)
	return c, ctx
}

func TestResolveAgent_RejectsInvalidSharedSelectorWithoutLocalFallback(t *testing.T) {
	c, ctx := newResolveAgentTestContext(7)
	localAgent := &types.CustomAgent{ID: "builtin-smart-reasoning", TenantID: 7, Name: "Local"}
	h := withAgentResolver(&Handler{
		agentShareService:  &resolveAgentShareStub{},
		customAgentService: &resolveOwnAgentStub{agent: localAgent},
	})

	agent, effectiveTenantID, sharedReadOnly := h.resolveAgent(ctx, c, "builtin-smart-reasoning", 84)

	require.Nil(t, agent)
	require.Equal(t, uint64(0), effectiveTenantID)
	require.False(t, sharedReadOnly)
}

func TestResolveAgent_UsesSharedAgentWhenSourceSelectorMatches(t *testing.T) {
	c, ctx := newResolveAgentTestContext(7)
	sharedAgent := &types.CustomAgent{ID: "builtin-smart-reasoning", TenantID: 84, Name: "Shared"}
	h := withAgentResolver(&Handler{
		agentShareService:  &resolveAgentShareStub{agent: sharedAgent},
		customAgentService: &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "builtin-smart-reasoning", TenantID: 7}},
	})

	agent, effectiveTenantID, sharedReadOnly := h.resolveAgent(ctx, c, "builtin-smart-reasoning", 84)

	require.Equal(t, sharedAgent, agent)
	require.Equal(t, uint64(84), effectiveTenantID)
	require.True(t, sharedReadOnly)
}

func TestResolveAgent_FallsBackToLocalAgentWithoutSourceSelector(t *testing.T) {
	c, ctx := newResolveAgentTestContext(7)
	localAgent := &types.CustomAgent{ID: "builtin-smart-reasoning", TenantID: 7, Name: "Local"}
	h := withAgentResolver(&Handler{
		agentShareService:  &resolveAgentShareStub{},
		customAgentService: &resolveOwnAgentStub{agent: localAgent},
	})

	agent, effectiveTenantID, sharedReadOnly := h.resolveAgent(ctx, c, "builtin-smart-reasoning", 0)

	require.Equal(t, localAgent, agent)
	require.Equal(t, uint64(0), effectiveTenantID)
	require.False(t, sharedReadOnly)
}

// Built-in IDs exist in every workspace. Without a source selector a share of
// the same ID must not replace the caller's own agent.
func TestResolveAgent_PrefersOwnAgentOverSameIDShareWithoutSourceSelector(t *testing.T) {
	c, ctx := newResolveAgentTestContext(7)
	localAgent := &types.CustomAgent{ID: "builtin-quick-answer", TenantID: 7, Name: "Local"}
	h := withAgentResolver(&Handler{
		agentShareService: &resolveAgentShareStub{
			agent: &types.CustomAgent{ID: "builtin-quick-answer", TenantID: 84, Name: "Shared"},
		},
		customAgentService: &resolveOwnAgentStub{agent: localAgent},
	})

	agent, effectiveTenantID, sharedReadOnly := h.resolveAgent(ctx, c, "builtin-quick-answer", 0)

	require.Equal(t, localAgent, agent)
	require.Equal(t, uint64(0), effectiveTenantID)
	require.False(t, sharedReadOnly)
}

// A shared agent without the source selector (e.g. a restored selection) still
// resolves when the caller has no agent of that ID.
func TestResolveAgent_UsesShareWhenNoOwnAgentMatches(t *testing.T) {
	c, ctx := newResolveAgentTestContext(7)
	sharedAgent := &types.CustomAgent{ID: "3f0c2d9e-shared", TenantID: 84, Name: "Shared"}
	h := withAgentResolver(&Handler{
		agentShareService:  &resolveAgentShareStub{agent: sharedAgent},
		customAgentService: &resolveOwnAgentStub{err: errors.New("agent not found")},
	})

	agent, effectiveTenantID, sharedReadOnly := h.resolveAgent(ctx, c, sharedAgent.ID, 0)

	require.Equal(t, sharedAgent, agent)
	require.Equal(t, uint64(84), effectiveTenantID)
	require.True(t, sharedReadOnly)
}

type resolveAgentExpectation int

const (
	expectNoAgent resolveAgentExpectation = iota
	expectOwnAgent
	expectSharedAgent
)

// resolveAgentCase is one input of the resolveAgent decision table. The table
// pins the chat behaviour that moved into service.AgentAccessResolver, so a
// later change to the resolver cannot shift what a conversation runs with.
type resolveAgentCase struct {
	name string
	// identity leaves the user or workspace key out of the request when nil.
	identity *resolveAgentIdentity
	agentID  string
	source   uint64
	own      *resolveOwnAgentStub
	share    *resolveAgentShareStub // nil = no share service wired
}

type resolveAgentIdentity struct {
	user   string
	tenant uint64
}

var resolveAgentMember = &resolveAgentIdentity{user: "user-1", tenant: 7}

var resolveAgentDecisionTable = []struct {
	resolveAgentCase
	want            resolveAgentExpectation
	effectiveTenant uint64 // effective tenant of a resolved share; own agents always resolve to 0
}{
	{
		resolveAgentCase: resolveAgentCase{
			name: "empty ID", identity: resolveAgentMember, agentID: "",
			own:   &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "own", TenantID: 7}},
			share: &resolveAgentShareStub{agent: &types.CustomAgent{ID: "shared", TenantID: 84}},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "own agent without a source", identity: resolveAgentMember, agentID: "agent",
			own:   &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "agent", TenantID: 7}},
			share: &resolveAgentShareStub{agent: &types.CustomAgent{ID: "agent", TenantID: 84}},
		},
		want: expectOwnAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "share when no own agent matches", identity: resolveAgentMember, agentID: "agent",
			own: &resolveOwnAgentStub{err: service.ErrAgentNotFound},
			share: &resolveAgentShareStub{
				agent: &types.CustomAgent{ID: "agent", TenantID: 84},
			},
		},
		want: expectSharedAgent, effectiveTenant: 84,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "source selector resolves the share", identity: resolveAgentMember, agentID: "agent", source: 84,
			own: &resolveOwnAgentStub{err: service.ErrAgentNotFound},
			share: &resolveAgentShareStub{
				agent: &types.CustomAgent{ID: "agent", TenantID: 84},
			},
		},
		want: expectSharedAgent, effectiveTenant: 84,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "source selector skips the same-ID own agent", identity: resolveAgentMember,
			agentID: "agent", source: 84,
			own: &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "agent", TenantID: 7}},
			share: &resolveAgentShareStub{
				agent: &types.CustomAgent{ID: "agent", TenantID: 84},
			},
		},
		want: expectSharedAgent, effectiveTenant: 84,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "refused selector never falls back to the own agent", identity: resolveAgentMember,
			agentID: "agent", source: 84,
			own:   &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "agent", TenantID: 7}},
			share: &resolveAgentShareStub{},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "revoked share with a source", identity: resolveAgentMember, agentID: "agent", source: 84,
			own:   &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "agent", TenantID: 7}},
			share: &resolveAgentShareStub{err: service.ErrAgentSharePermission},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "built-in ID stays in the caller's workspace", identity: resolveAgentMember,
			agentID: "builtin-quick-answer",
			own:     &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "builtin-quick-answer", TenantID: 7}},
			share: &resolveAgentShareStub{
				agent: &types.CustomAgent{ID: "builtin-quick-answer", TenantID: 84},
			},
		},
		want: expectOwnAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "no agent anywhere", identity: resolveAgentMember, agentID: "agent",
			own:   &resolveOwnAgentStub{err: service.ErrAgentNotFound},
			share: &resolveAgentShareStub{err: service.ErrAgentSharePermission},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "share without a user", identity: &resolveAgentIdentity{tenant: 7}, agentID: "agent",
			own: &resolveOwnAgentStub{err: service.ErrAgentNotFound},
			share: &resolveAgentShareStub{
				agent: &types.CustomAgent{ID: "agent", TenantID: 84},
			},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "share without a workspace", identity: &resolveAgentIdentity{user: "user-1"}, agentID: "agent",
			own: &resolveOwnAgentStub{err: service.ErrAgentNotFound},
			share: &resolveAgentShareStub{
				agent: &types.CustomAgent{ID: "agent", TenantID: 84},
			},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "no share service wired", identity: resolveAgentMember, agentID: "agent",
			own: &resolveOwnAgentStub{err: service.ErrAgentNotFound},
		},
		want: expectNoAgent,
	},
	{
		resolveAgentCase: resolveAgentCase{
			name: "share store failure", identity: resolveAgentMember, agentID: "agent",
			own:   &resolveOwnAgentStub{err: service.ErrAgentNotFound},
			share: &resolveAgentShareStub{err: errors.New("share store down")},
		},
		want: expectNoAgent,
	},
}

// handler wires the case's stubs the way NewHandler wires the services: a nil
// share stub leaves no share service at all, rather than a typed nil one.
func (tc resolveAgentCase) handler() *Handler {
	h := &Handler{customAgentService: tc.own}
	if tc.share != nil {
		h.agentShareService = tc.share
	}
	return withAgentResolver(h)
}

// withAgentResolver wires the agent resolver from h's agent services, the way
// the container hands NewHandler the AgentAccessResolver built from them.
func withAgentResolver(h *Handler) *Handler {
	h.agentAccessResolver = service.NewAgentAccessResolver(h.customAgentService, h.agentShareService, nil, nil, nil)
	return h
}

// Without a resolver the chat path resolves nothing and falls back to the
// default agent config; it never reaches for a share on its own.
func TestResolveAgent_WithoutResolverResolvesNothing(t *testing.T) {
	tc := resolveAgentCase{identity: resolveAgentMember}
	c, ctx := tc.request()
	h := &Handler{
		customAgentService: &resolveOwnAgentStub{agent: &types.CustomAgent{ID: "agent", TenantID: 7}},
		agentShareService:  &resolveAgentShareStub{agent: &types.CustomAgent{ID: "agent", TenantID: 84}},
	}

	agent, effectiveTenantID, sharedReadOnly := h.resolveAgent(ctx, c, "agent", 0)

	require.Nil(t, agent)
	require.Zero(t, effectiveTenantID)
	require.False(t, sharedReadOnly)
}

func (tc resolveAgentCase) request() (*gin.Context, context.Context) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ctx := c.Request.Context()
	if tc.identity != nil {
		if tc.identity.user != "" {
			c.Set(types.UserIDContextKey.String(), tc.identity.user)
		}
		if tc.identity.tenant != 0 {
			c.Set(types.TenantIDContextKey.String(), tc.identity.tenant)
			ctx = context.WithValue(ctx, types.TenantIDContextKey, tc.identity.tenant)
		}
	}
	c.Request = c.Request.WithContext(ctx)
	return c, ctx
}

func TestResolveAgent_DecisionTable(t *testing.T) {
	for _, tc := range resolveAgentDecisionTable {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := tc.request()

			agent, effectiveTenantID, sharedReadOnly := tc.handler().resolveAgent(ctx, c, tc.agentID, tc.source)

			switch tc.want {
			case expectOwnAgent:
				require.Same(t, tc.own.agent, agent)
				require.Zero(t, effectiveTenantID, "an own agent runs in the caller's workspace")
				require.False(t, sharedReadOnly)
			case expectSharedAgent:
				require.Same(t, tc.share.agent, agent)
				require.Equal(t, tc.effectiveTenant, effectiveTenantID)
				require.True(t, sharedReadOnly)
			default:
				require.Nil(t, agent)
				require.Zero(t, effectiveTenantID)
				require.False(t, sharedReadOnly)
			}
		})
	}
}

func TestTerminalProvisionPin_UsesSharedAgentSandboxConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodGet,
		"/?agent_id=builtin-smart-reasoning&agent_source_tenant_id=84",
		nil,
	)
	c.Set(types.UserIDContextKey.String(), "user-1")
	c.Set(types.TenantIDContextKey.String(), uint64(7))
	ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(7))
	c.Request = c.Request.WithContext(ctx)

	sharedAgent := &types.CustomAgent{
		ID:       "builtin-smart-reasoning",
		TenantID: 84,
		Config:   types.CustomAgentConfig{SandboxConfigID: "shared-cfg"},
	}
	h := withAgentResolver(&Handler{
		agentShareService: &resolveAgentShareStub{agent: sharedAgent},
		customAgentService: &resolveOwnAgentStub{agent: &types.CustomAgent{
			ID:       "builtin-smart-reasoning",
			TenantID: 7,
			Config:   types.CustomAgentConfig{SandboxConfigID: "local-cfg"},
		}},
	})

	require.Equal(t, service.SandboxPin{}, h.terminalProvisionPin(ctx, c, false),
		"lookup-only connects must not resolve an agent config")
	// The lending workspace travels with the config id: sandbox configs are
	// keyed by (tenant, id), so "shared-cfg" does not exist in workspace 7.
	require.Equal(t,
		service.SandboxPin{ConfigID: "shared-cfg", TenantID: 84},
		h.terminalProvisionPin(ctx, c, true))
}

// An own agent's config already lives in the caller's workspace, so the pin
// carries no owner and SandboxPin.TenantOr falls back to the request tenant.
func TestTerminalProvisionPin_OwnAgentCarriesNoWorkspace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodGet, "/?agent_id=builtin-smart-reasoning", nil)
	c.Set(types.UserIDContextKey.String(), "user-1")
	c.Set(types.TenantIDContextKey.String(), uint64(7))
	ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(7))
	c.Request = c.Request.WithContext(ctx)

	h := withAgentResolver(&Handler{
		customAgentService: &resolveOwnAgentStub{agent: &types.CustomAgent{
			ID:       "builtin-smart-reasoning",
			TenantID: 7,
			Config:   types.CustomAgentConfig{SandboxConfigID: "local-cfg"},
		}},
	})

	pin := h.terminalProvisionPin(ctx, c, true)
	require.Equal(t, service.SandboxPin{ConfigID: "local-cfg"}, pin)
	require.Equal(t, uint64(7), pin.TenantOr(7))
}
