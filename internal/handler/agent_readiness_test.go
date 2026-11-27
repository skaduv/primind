package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/agent/tools"
	"github.com/primind/primind/internal/application/service"
	apperrors "github.com/primind/primind/internal/errors"
	"github.com/primind/primind/internal/middleware"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type notReadyAgentShares struct {
	interfaces.AgentShareService
	share *types.AgentShare
	err   error
}

func (s *notReadyAgentShares) ShareAgent(
	context.Context, string, string, string, uint64, types.OrgMemberRole,
) (*types.AgentShare, error) {
	return s.share, s.err
}

// agentNotConfiguredMessage is the text a share refusal always carried. It must
// not change: an older client shows it verbatim (C1).
const agentNotConfiguredMessage = "Agent is not fully configured. " +
	"Please set the chat model, and set the rerank model if the knowledge_search tool is enabled in agent settings."

// A share refused for an incomplete agent answers as it did before, and
// additionally names what is missing in details.issues.
func TestShareAgentNotConfiguredResponse(t *testing.T) {
	issues := []types.AgentReadinessIssue{
		{Code: types.AgentIssueMissingRerankModel, Field: types.AgentFieldRerankModel},
	}
	for _, tc := range []struct {
		name      string
		err       error
		wantIssue bool
	}{
		{"issues from the readiness check", &service.AgentNotReadyError{Issues: issues}, true},
		{"bare sentinel carries no details", service.ErrAgentNotConfigured, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.Use(middleware.ErrorHandler())
			h := &OrganizationHandler{agentShareService: &notReadyAgentShares{err: tc.err}}
			router.POST("/agents/:id/shares", h.ShareAgent)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/agents/agent-1/shares",
				strings.NewReader(`{"organization_id":"org-1","permission":"viewer"}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var body struct {
				Success bool `json:"success"`
				Error   struct {
					Code    apperrors.ErrorCode        `json:"code"`
					Message string                     `json:"message"`
					Details map[string]json.RawMessage `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.False(t, body.Success)
			require.Equal(t, apperrors.ErrValidation, body.Error.Code)
			require.Equal(t, agentNotConfiguredMessage, body.Error.Message)

			raw, ok := body.Error.Details["issues"]
			if !tc.wantIssue {
				require.False(t, ok, "no issues to report: %s", rec.Body.String())
				return
			}
			require.True(t, ok, rec.Body.String())
			var got []types.AgentReadinessIssue
			require.NoError(t, json.Unmarshal(raw, &got))
			require.Equal(t, issues, got)
		})
	}
}

type readinessAgentRepo struct {
	interfaces.CustomAgentRepository
	agent *types.CustomAgent
}

func (r *readinessAgentRepo) GetAgentByID(context.Context, string, uint64) (*types.CustomAgent, error) {
	return r.agent, nil
}

func (r *readinessAgentRepo) UpdateAgent(_ context.Context, agent *types.CustomAgent) error {
	r.agent = agent
	return nil
}

type readinessAgentShareRepo struct {
	interfaces.AgentShareRepository
	shares []*types.AgentShare
}

func (r *readinessAgentShareRepo) ListByAgent(context.Context, string) ([]*types.AgentShare, error) {
	return r.shares, nil
}

type handlerReadinessModels struct {
	interfaces.ModelRepository
	models map[string]*types.Model
}

func (r *handlerReadinessModels) GetByID(_ context.Context, _ uint64, id string) (*types.Model, error) {
	return r.models[id], nil
}

type saveWarning struct {
	Code   string                      `json:"code"`
	Issues []types.AgentReadinessIssue `json:"issues"`
}

func warningsOf(t *testing.T, response map[string]json.RawMessage) []saveWarning {
	t.Helper()
	raw, ok := response["warnings"]
	if !ok {
		return nil
	}
	var warnings []saveWarning
	require.NoError(t, json.Unmarshal(raw, &warnings))
	return warnings
}

// putAgent runs PUT /agents/:id through the real handler and error middleware.
func putAgent(t *testing.T, svc interfaces.CustomAgentService, body string) map[string]json.RawMessage {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.ErrorHandler())
	router.PUT("/agents/:id", (&CustomAgentHandler{service: svc}).UpdateAgent)

	req := httptest.NewRequest(http.MethodPut, "/agents/agent-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), types.TenantIDContextKey, uint64(1)))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	return response
}

// Clearing the rerank model of a shared agent still saves (C3) and comes back
// with a warning; the same edit on an unshared agent does not.
func TestUpdateAgentWarnsWhenASharedAgentIsNotReady(t *testing.T) {
	active := map[string]*types.Model{
		"chat-1":   {ID: "chat-1", Type: types.ModelTypeKnowledgeQA, Status: types.ModelStatusActive},
		"rerank-1": {ID: "rerank-1", Type: types.ModelTypeRerank, Status: types.ModelStatusActive},
	}
	shared := []*types.AgentShare{{ID: "share-1", AgentID: "agent-1", SourceTenantID: 1}}
	rerankCleared := `{"name":"agent","config":{"kb_selection_mode":"selected","knowledge_bases":["kb-1"],` +
		`"allowed_tools":["search_knowledge"],"model_id":"chat-1"}}`
	complete := `{"name":"agent","config":{"kb_selection_mode":"selected","knowledge_bases":["kb-1"],` +
		`"allowed_tools":["search_knowledge"],"model_id":"chat-1","rerank_model_id":"rerank-1"}}`
	storedConfig := types.CustomAgentConfig{
		ModelID: "chat-1", RerankModelID: "rerank-1",
		KBSelectionMode: "selected", KnowledgeBases: []string{"kb-1"},
		AllowedTools: []string{tools.ToolSearchKnowledge},
	}
	// A real agent service over stub repositories: the handler reaches the
	// readiness check through it, the way production does.
	newService := func(shares []*types.AgentShare, models map[string]*types.Model) interfaces.CustomAgentService {
		repo := &readinessAgentRepo{agent: &types.CustomAgent{
			ID: "agent-1", TenantID: 1, Name: "agent", Config: storedConfig,
		}}
		return service.NewCustomAgentService(
			repo, nil, nil, nil, nil, nil, nil,
			&readinessAgentShareRepo{shares: shares}, &handlerReadinessModels{models: models},
		)
	}
	rerankWarning := []saveWarning{{
		Code: types.AgentWarningSharedNotReady,
		Issues: []types.AgentReadinessIssue{
			{Code: types.AgentIssueMissingRerankModel, Field: types.AgentFieldRerankModel},
		},
	}}

	t.Run("shared agent, rerank model cleared", func(t *testing.T) {
		response := putAgent(t, newService(shared, active), rerankCleared)
		require.Equal(t, rerankWarning, warningsOf(t, response))
	})

	t.Run("shared agent, configuration complete", func(t *testing.T) {
		response := putAgent(t, newService(shared, active), complete)
		require.Empty(t, warningsOf(t, response))
	})

	t.Run("agent nobody shares", func(t *testing.T) {
		response := putAgent(t, newService(nil, active), rerankCleared)
		_, hasWarnings := response["warnings"]
		require.False(t, hasWarnings, "an unshared agent is saved without the warnings field: %s", response["data"])
	})

	t.Run("shared agent, saved chat model deleted", func(t *testing.T) {
		response := putAgent(t, newService(shared, map[string]*types.Model{"rerank-1": active["rerank-1"]}), complete)
		require.Equal(t, []saveWarning{{
			Code: types.AgentWarningSharedNotReady,
			Issues: []types.AgentReadinessIssue{
				{Code: types.AgentIssueChatModelUnavailable, Field: types.AgentFieldChatModel},
			},
		}}, warningsOf(t, response))
	})

	t.Run("a service without the check reports no warning", func(t *testing.T) {
		svc := &savingAgentService{stored: &types.CustomAgent{ID: "agent-1", Config: storedConfig}}
		response := putAgent(t, svc, rerankCleared)
		require.Empty(t, warningsOf(t, response))
	})
}
