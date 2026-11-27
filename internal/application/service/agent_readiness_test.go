package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/primind/primind/internal/agent/tools"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// readinessModelRepo answers lookups from a map the way the real repository
// does: no model is nil without an error.
type readinessModelRepo struct {
	interfaces.ModelRepository
	models  map[string]*types.Model
	err     error
	lookups int
	tenant  uint64
}

func (r *readinessModelRepo) GetByID(_ context.Context, tenantID uint64, id string) (*types.Model, error) {
	r.lookups++
	r.tenant = tenantID
	if r.err != nil {
		return nil, r.err
	}
	return r.models[id], nil
}

func activeReadinessModel(id string, modelType types.ModelType) *types.Model {
	return &types.Model{ID: id, TenantID: 42, Type: modelType, Status: types.ModelStatusActive}
}

// readinessAgent runs knowledge search, so it needs both a chat and a rerank
// model. mutate changes one aspect of its saved configuration.
func readinessAgent(mutate func(*types.CustomAgentConfig)) *types.CustomAgent {
	config := types.CustomAgentConfig{
		ModelID:         "chat-1",
		RerankModelID:   "rerank-1",
		KBSelectionMode: "selected",
		KnowledgeBases:  []string{"kb-1"},
		AllowedTools:    []string{tools.ToolSearchKnowledge},
	}
	if mutate != nil {
		mutate(&config)
	}
	return &types.CustomAgent{ID: "agent-1", TenantID: 42, Config: config}
}

// oneReadinessIssue is the issue list of a check that found exactly one problem.
func oneReadinessIssue(code, field string) []types.AgentReadinessIssue {
	return []types.AgentReadinessIssue{readinessIssue(code, field)}
}

func readinessIssue(code, field string) types.AgentReadinessIssue {
	return types.AgentReadinessIssue{Code: code, Field: field}
}

func TestCheckAgentReadiness(t *testing.T) {
	activeModels := map[string]*types.Model{
		"chat-1":   activeReadinessModel("chat-1", types.ModelTypeKnowledgeQA),
		"rerank-1": activeReadinessModel("rerank-1", types.ModelTypeRerank),
	}
	withModels := func(models map[string]*types.Model) *readinessModelRepo {
		return &readinessModelRepo{models: models}
	}
	noChat := func(c *types.CustomAgentConfig) { c.ModelID = "" }
	noRerank := func(c *types.CustomAgentConfig) { c.RerankModelID = "" }
	noKnowledge := func(c *types.CustomAgentConfig) { c.KBSelectionMode = "none"; c.KnowledgeBases = nil }

	cases := []struct {
		name   string
		agent  *types.CustomAgent
		models ModelLookup
		want   []types.AgentReadinessIssue
	}{
		{
			name:   "configured completely",
			agent:  readinessAgent(nil),
			models: withModels(activeModels),
		},
		{
			name:   "no chat model",
			agent:  readinessAgent(noChat),
			models: withModels(activeModels),
			want:   oneReadinessIssue(types.AgentIssueMissingChatModel, types.AgentFieldChatModel),
		},
		{
			name:   "knowledge search without a rerank model",
			agent:  readinessAgent(noRerank),
			models: withModels(activeModels),
			want:   oneReadinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
		},
		{
			name:   "knowledge bases disabled: the rerank model is optional",
			agent:  readinessAgent(func(c *types.CustomAgentConfig) { noRerank(c); noKnowledge(c) }),
			models: withModels(activeModels),
		},
		{
			name: "search tool not allowed: the rerank model is optional",
			agent: readinessAgent(func(c *types.CustomAgentConfig) {
				noRerank(c)
				c.AllowedTools = []string{"thinking"}
			}),
			models: withModels(activeModels),
		},
		{
			name:   "chat model no longer exists",
			agent:  readinessAgent(nil),
			models: withModels(map[string]*types.Model{"rerank-1": activeModels["rerank-1"]}),
			want:   oneReadinessIssue(types.AgentIssueChatModelUnavailable, types.AgentFieldChatModel),
		},
		{
			name:  "chat model is not active",
			agent: readinessAgent(nil),
			models: withModels(map[string]*types.Model{
				"chat-1":   {ID: "chat-1", Type: types.ModelTypeKnowledgeQA, Status: types.ModelStatusDownloadFailed},
				"rerank-1": activeModels["rerank-1"],
			}),
			want: oneReadinessIssue(types.AgentIssueChatModelUnavailable, types.AgentFieldChatModel),
		},
		{
			name:   "rerank model no longer exists",
			agent:  readinessAgent(nil),
			models: withModels(map[string]*types.Model{"chat-1": activeModels["chat-1"]}),
			want:   oneReadinessIssue(types.AgentIssueRerankModelUnavailable, types.AgentFieldRerankModel),
		},
		{
			name:   "a stale rerank model is ignored where nothing reranks",
			agent:  readinessAgent(noKnowledge),
			models: withModels(map[string]*types.Model{"chat-1": activeModels["chat-1"]}),
		},
		{
			name:   "everything missing is reported together, chat model first",
			agent:  readinessAgent(func(c *types.CustomAgentConfig) { noChat(c); noRerank(c) }),
			models: withModels(activeModels),
			want: []types.AgentReadinessIssue{
				readinessIssue(types.AgentIssueMissingChatModel, types.AgentFieldChatModel),
				readinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
			},
		},
		{
			name:   "a failed lookup is not held against the agent",
			agent:  readinessAgent(nil),
			models: &readinessModelRepo{err: errors.New("db unavailable")},
		},
		{
			name:  "a failed lookup does not hide a missing id",
			agent: readinessAgent(noRerank),
			models: &readinessModelRepo{
				models: map[string]*types.Model{},
				err:    errors.New("db unavailable"),
			},
			want: oneReadinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
		},
		{
			name:   "without a lookup only missing ids are checked",
			agent:  readinessAgent(nil),
			models: nil,
		},
		{
			name:  "nil agent",
			agent: nil,
			want:  oneReadinessIssue(types.AgentIssueMissingChatModel, types.AgentFieldChatModel),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil interface, not a nil *readinessModelRepo: CheckAgentReadiness
			// tells "no lookup" apart by the interface being nil.
			var lookup ModelLookup
			if tc.models != nil {
				lookup = tc.models
			}
			got := CheckAgentReadiness(context.Background(), tc.agent, lookup)
			require.Equal(t, tc.want, got)
			if repo, ok := lookup.(*readinessModelRepo); ok && repo.lookups > 0 {
				require.Equal(t, uint64(42), repo.tenant, "models are resolved in the agent's workspace")
			}
		})
	}
}

// Where nothing reranks, the rerank model is not looked up at all.
func TestCheckAgentReadinessLookupCount(t *testing.T) {
	models := &readinessModelRepo{models: map[string]*types.Model{
		"chat-1": activeReadinessModel("chat-1", types.ModelTypeKnowledgeQA),
	}}
	_ = CheckAgentReadiness(context.Background(), readinessAgent(func(c *types.CustomAgentConfig) {
		c.RerankModelID = ""
		c.KBSelectionMode = "none"
		c.KnowledgeBases = nil
	}), models)
	require.Equal(t, 1, models.lookups, "the unused rerank model is not looked up")
}

type readinessOrgRepo struct {
	interfaces.OrganizationRepository
}

func (readinessOrgRepo) GetByID(context.Context, string) (*types.Organization, error) {
	return &types.Organization{ID: "org-1"}, nil
}

func (readinessOrgRepo) GetTenantMember(context.Context, string, uint64) (*types.OrganizationTenantMember, error) {
	return &types.OrganizationTenantMember{Role: types.OrgRoleEditor}, nil
}

type readinessShareRepo struct {
	interfaces.AgentShareRepository
	created []*types.AgentShare
}

func (r *readinessShareRepo) Create(_ context.Context, share *types.AgentShare) error {
	r.created = append(r.created, share)
	return nil
}

// readinessKBRepo only needs to be non-nil: the caller is a workspace admin,
// so the KB scope check returns before any lookup.
type readinessKBRepo struct {
	interfaces.KnowledgeBaseRepository
}

func shareReadinessService(
	agent *types.CustomAgent, models *readinessModelRepo,
) (*agentShareService, *readinessShareRepo) {
	shares := &readinessShareRepo{}
	return &agentShareService{
		agentRepo: &builtinShareAgentRepo{agent: agent},
		orgRepo:   readinessOrgRepo{},
		shareRepo: shares,
		kbRepo:    readinessKBRepo{},
		modelRepo: models,
	}, shares
}

// A failed share still answers ErrAgentNotConfigured, and names what is wrong
// with the saved configuration.
func TestShareAgentReportsWhatIsNotConfigured(t *testing.T) {
	ctx := context.WithValue(context.Background(), types.TenantRoleContextKey, types.TenantRoleAdmin)
	active := map[string]*types.Model{
		"chat-1":   activeReadinessModel("chat-1", types.ModelTypeKnowledgeQA),
		"rerank-1": activeReadinessModel("rerank-1", types.ModelTypeRerank),
	}
	cases := []struct {
		name   string
		agent  *types.CustomAgent
		models map[string]*types.Model
		want   []types.AgentReadinessIssue
	}{
		{
			name:   "rerank model never saved",
			agent:  readinessAgent(func(c *types.CustomAgentConfig) { c.RerankModelID = "" }),
			models: active,
			want:   oneReadinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
		},
		{
			name:   "no model at all",
			agent:  readinessAgent(func(c *types.CustomAgentConfig) { c.ModelID = ""; c.RerankModelID = "" }),
			models: active,
			want: []types.AgentReadinessIssue{
				readinessIssue(types.AgentIssueMissingChatModel, types.AgentFieldChatModel),
				readinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
			},
		},
		{
			name:   "the saved chat model was deleted",
			agent:  readinessAgent(nil),
			models: map[string]*types.Model{"rerank-1": active["rerank-1"]},
			want:   oneReadinessIssue(types.AgentIssueChatModelUnavailable, types.AgentFieldChatModel),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, shares := shareReadinessService(tc.agent, &readinessModelRepo{models: tc.models})

			share, err := svc.ShareAgent(ctx, "agent-1", "org-1", "user-1", 42, types.OrgRoleViewer)

			require.Nil(t, share)
			require.ErrorIs(t, err, ErrAgentNotConfigured)
			var notReady *AgentNotReadyError
			require.ErrorAs(t, err, &notReady)
			require.Equal(t, tc.want, notReady.Issues)
			require.Empty(t, shares.created)
		})
	}
}

// A ready agent is shared in the same single request as before, including
// when the knowledge base is disabled and no rerank model is set (C2, C4).
func TestShareAgentAcceptsAReadyAgent(t *testing.T) {
	ctx := context.WithValue(context.Background(), types.TenantRoleContextKey, types.TenantRoleAdmin)
	for _, tc := range []struct {
		name   string
		agent  *types.CustomAgent
		models map[string]*types.Model
	}{
		{
			name:  "chat and rerank model",
			agent: readinessAgent(nil),
			models: map[string]*types.Model{
				"chat-1":   activeReadinessModel("chat-1", types.ModelTypeKnowledgeQA),
				"rerank-1": activeReadinessModel("rerank-1", types.ModelTypeRerank),
			},
		},
		{
			name: "knowledge bases disabled, rerank model empty",
			agent: readinessAgent(func(c *types.CustomAgentConfig) {
				c.RerankModelID = ""
				c.KBSelectionMode = "none"
				c.KnowledgeBases = nil
			}),
			models: map[string]*types.Model{"chat-1": activeReadinessModel("chat-1", types.ModelTypeKnowledgeQA)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, shares := shareReadinessService(tc.agent, &readinessModelRepo{models: tc.models})

			share, err := svc.ShareAgent(ctx, "agent-1", "org-1", "user-1", 42, types.OrgRoleViewer)

			require.NoError(t, err)
			require.NotNil(t, share)
			require.Len(t, shares.created, 1)
			require.Equal(t, "org-1", shares.created[0].OrganizationID)
		})
	}
}

// The receiver list marks what receivers cannot use, from stored data only.
func TestSharedAgentsListReadiness(t *testing.T) {
	notReady := readinessAgent(func(c *types.CustomAgentConfig) { c.RerankModelID = "" })
	notReady.ID = "not-ready"
	ready := readinessAgent(nil)
	ready.ID = "ready"
	models := &readinessModelRepo{models: map[string]*types.Model{}}
	svc := &agentShareService{
		orgRepo:      receiverViewOrgRepo{},
		disabledRepo: receiverViewDisabledRepo{},
		shareRepo: receiverViewTenantShareRepo{shares: []*types.AgentShare{
			{AgentID: "not-ready", SourceTenantID: 84, Permission: types.OrgRoleViewer, Agent: notReady},
			{AgentID: "ready", SourceTenantID: 84, Permission: types.OrgRoleViewer, Agent: ready},
		}},
		modelRepo: models,
	}

	infos, err := svc.ListSharedAgents(context.Background(), 7, types.TenantRoleAdmin)

	require.NoError(t, err)
	require.Len(t, infos, 2)
	for _, info := range infos {
		switch info.Agent.ID {
		case "not-ready":
			require.False(t, info.Ready)
			require.Equal(t, []types.AgentReadinessIssue{
				readinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
			}, info.Issues)
		case "ready":
			require.True(t, info.Ready)
			require.Empty(t, info.Issues)
		default:
			t.Fatalf("unexpected agent %q", info.Agent.ID)
		}
	}
	require.Zero(t, models.lookups, "the list must not query the model table")
}

// The fields a client reads off a shared-agent list item.
func TestSharedAgentInfoReadinessJSON(t *testing.T) {
	notReady, err := json.Marshal(types.SharedAgentInfo{
		Issues: oneReadinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
	})
	require.NoError(t, err)
	require.Contains(t, string(notReady), `"ready":false`)
	require.Contains(t, string(notReady), `"issues":[{"code":"missing_rerank_model","field":"config.rerank_model_id"}]`)

	ready, err := json.Marshal(types.SharedAgentInfo{Ready: true})
	require.NoError(t, err)
	require.Contains(t, string(ready), `"ready":true`)
	require.NotContains(t, string(ready), `"issues"`)
}

type readinessAgentShareRepo struct {
	interfaces.AgentShareRepository
	shares []*types.AgentShare
	err    error
}

func (r *readinessAgentShareRepo) ListByAgent(context.Context, string) ([]*types.AgentShare, error) {
	return r.shares, r.err
}

func sharedBy(t *testing.T, sourceTenantID uint64) []*types.AgentShare {
	t.Helper()
	return []*types.AgentShare{{AgentID: "agent-1", SourceTenantID: sourceTenantID}}
}

// After a save, only an agent this workspace shares is re-checked, and the
// result is a warning rather than a failure.
func TestSharedAgentReadinessIssuesAfterSave(t *testing.T) {
	ctx := context.Background()
	ready := readinessAgent(nil)
	rerankCleared := readinessAgent(func(c *types.CustomAgentConfig) { c.RerankModelID = "" })
	active := func() map[string]*types.Model {
		return map[string]*types.Model{
			"chat-1":   activeReadinessModel("chat-1", types.ModelTypeKnowledgeQA),
			"rerank-1": activeReadinessModel("rerank-1", types.ModelTypeRerank),
		}
	}

	shared := &customAgentService{
		agentShareRepo: &readinessAgentShareRepo{shares: sharedBy(t, 42)},
		modelRepo:      &readinessModelRepo{models: active()},
	}
	require.Equal(t, []types.AgentReadinessIssue{
		readinessIssue(types.AgentIssueMissingRerankModel, types.AgentFieldRerankModel),
	}, shared.SharedAgentReadinessIssues(ctx, rerankCleared), "clearing the rerank model of a shared agent warns")
	require.Empty(t, shared.SharedAgentReadinessIssues(ctx, ready), "a complete configuration does not warn")

	deletedChat := &customAgentService{
		agentShareRepo: &readinessAgentShareRepo{shares: sharedBy(t, 42)},
		modelRepo:      &readinessModelRepo{models: map[string]*types.Model{"rerank-1": active()["rerank-1"]}},
	}
	require.Equal(t, []types.AgentReadinessIssue{
		readinessIssue(types.AgentIssueChatModelUnavailable, types.AgentFieldChatModel),
	}, deletedChat.SharedAgentReadinessIssues(ctx, ready), "a model deleted since the agent was configured warns")

	unshared := &customAgentService{
		agentShareRepo: &readinessAgentShareRepo{},
		modelRepo:      &readinessModelRepo{models: active()},
	}
	require.Empty(t, unshared.SharedAgentReadinessIssues(ctx, rerankCleared), "an agent nobody shares does not warn")

	// Shares of other workspaces are merely listed for this one; the agent
	// is not this workspace's to fix.
	receivedOnly := &customAgentService{
		agentShareRepo: &readinessAgentShareRepo{shares: sharedBy(t, 7)},
		modelRepo:      &readinessModelRepo{models: active()},
	}
	require.Empty(t, receivedOnly.SharedAgentReadinessIssues(ctx, rerankCleared))

	failing := &customAgentService{
		agentShareRepo: &readinessAgentShareRepo{err: errors.New("db unavailable")},
		modelRepo:      &readinessModelRepo{models: active()},
	}
	require.Empty(t, failing.SharedAgentReadinessIssues(ctx, rerankCleared), "a failed lookup never fails the save")

	legacyBuiltinShare := &types.CustomAgent{
		ID: types.BuiltinQuickAnswerID, IsBuiltin: true, TenantID: 42,
		Config: types.CustomAgentConfig{ModelID: ""},
	}
	require.Empty(t, shared.SharedAgentReadinessIssues(ctx, legacyBuiltinShare),
		"built-in agents are never resolved for receivers")
	require.Empty(t, shared.SharedAgentReadinessIssues(ctx, nil))
}
