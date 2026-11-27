package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/application/service"
	apperrors "github.com/primind/primind/internal/errors"
	"github.com/primind/primind/internal/middleware"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type suggestedQuestionsAgentService struct {
	interfaces.CustomAgentService
	err       error
	tagScopes []types.TagScope
}

func (s *suggestedQuestionsAgentService) GetAgentByID(context.Context, string) (*types.CustomAgent, error) {
	return &types.CustomAgent{ID: "agent-1", TenantID: 1}, nil
}

func (s *suggestedQuestionsAgentService) GetSuggestedQuestions(
	_ context.Context,
	_ string,
	_ []string,
	_ []string,
	tagScopes []types.TagScope,
	_ int,
) ([]types.SuggestedQuestion, error) {
	s.tagScopes = tagScopes
	return nil, s.err
}

// newSuggestedQuestionsHandler wires the service behind the same agent
// resolution the production handler uses, without a share service.
func newSuggestedQuestionsHandler(svc interfaces.CustomAgentService) *CustomAgentHandler {
	return &CustomAgentHandler{
		service:             svc,
		agentAccessResolver: service.NewAgentAccessResolver(svc, nil, nil, nil, nil),
	}
}

func TestGetSuggestedQuestionsPreservesAppErrorStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())

	h := newSuggestedQuestionsHandler(&suggestedQuestionsAgentService{
		err: apperrors.NewForbiddenError("API key scope does not allow one or more knowledge bases"),
	})
	r.GET("/agents/:id/suggested-questions", h.GetSuggestedQuestions)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/suggested-questions?knowledge_base_ids=kb-blocked", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestGetSuggestedQuestionsParsesScopedTags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())

	svc := &suggestedQuestionsAgentService{}
	h := newSuggestedQuestionsHandler(svc)
	r.GET("/agents/:id/suggested-questions", h.GetSuggestedQuestions)

	rawScopes := `[{"knowledge_base_id":"kb-1","tag_ids":["tag-1","tag-2"]}]`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/agents/agent-1/suggested-questions?tag_scopes="+url.QueryEscape(rawScopes),
		nil,
	)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []types.TagScope{{KnowledgeBaseID: "kb-1", TagIDs: []string{"tag-1", "tag-2"}}}, svc.tagScopes)
}

func TestGetSuggestedQuestionsRejectsInvalidScopedTags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler())

	h := newSuggestedQuestionsHandler(&suggestedQuestionsAgentService{})
	r.GET("/agents/:id/suggested-questions", h.GetSuggestedQuestions)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/agents/agent-1/suggested-questions?tag_scopes="+url.QueryEscape("not-json"),
		nil,
	)
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
