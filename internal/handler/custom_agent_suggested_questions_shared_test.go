package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/application/access"
	"github.com/primind/primind/internal/application/service"
	"github.com/primind/primind/internal/middleware"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

const (
	suggestionCallerTenant = uint64(1)
	suggestionSourceTenant = uint64(84)
	suggestionAgentID      = "shared-agent"
)

// suggestionAgentService stands in for the agent service on both ends of the
// handler: the own-agent lookup the resolver performs and the suggestion call.
type suggestionAgentService struct {
	interfaces.CustomAgentService
	own       *types.CustomAgent
	ownErr    error
	ownCalls  int
	questions []types.SuggestedQuestion
	err       error

	calls     int
	gotKBs    []string
	gotDocs   []string
	gotTags   []types.TagScope
	gotLimit  int
	gotTenant uint64
	gotCaller uint64
	gotGrant  bool
}

func (s *suggestionAgentService) GetAgentByID(context.Context, string) (*types.CustomAgent, error) {
	s.ownCalls++
	return s.own, s.ownErr
}

func (s *suggestionAgentService) GetSuggestedQuestions(
	ctx context.Context,
	_ string,
	kbIDs []string,
	knowledgeIDs []string,
	tagScopes []types.TagScope,
	limit int,
) ([]types.SuggestedQuestion, error) {
	s.calls++
	s.gotKBs, s.gotDocs, s.gotTags, s.gotLimit = kbIDs, knowledgeIDs, tagScopes, limit
	s.gotTenant, _ = types.TenantIDFromContext(ctx)
	s.gotCaller = types.CallerFromContext(ctx).TenantID
	s.gotGrant = access.HasKBGrant(ctx, "kb-in", suggestionSourceTenant, types.OrgRoleViewer)
	return s.questions, s.err
}

type suggestionAgentShares struct {
	interfaces.AgentShareService
	agent  *types.CustomAgent
	err    error
	calls  int
	tenant uint64
	role   types.TenantRole
	source uint64
}

func (s *suggestionAgentShares) GetSharedAgentForTenant(
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

type suggestionDisabledList struct {
	interfaces.TenantDisabledSharedAgentRepository
	records []*types.TenantDisabledSharedAgent
	err     error
}

func (s suggestionDisabledList) ListByTenantID(
	context.Context, uint64,
) ([]*types.TenantDisabledSharedAgent, error) {
	return s.records, s.err
}

type suggestionKnowledge struct {
	interfaces.KnowledgeRepository
	byID map[string]*types.Knowledge
}

func (s suggestionKnowledge) GetKnowledgeByIDOnly(_ context.Context, id string) (*types.Knowledge, error) {
	knowledge, ok := s.byID[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return knowledge, nil
}

func suggestionSharedAgent() *types.CustomAgent {
	return &types.CustomAgent{
		ID:       suggestionAgentID,
		TenantID: suggestionSourceTenant,
		Config: types.CustomAgentConfig{
			KBSelectionMode: "selected",
			KnowledgeBases:  []string{"kb-in"},
		},
	}
}

func suggestionSuggestions() []types.SuggestedQuestion {
	return []types.SuggestedQuestion{{Question: "What is the leave policy?", Source: "faq", KnowledgeBaseID: "kb-in"}}
}

// suggestionOwnAgent is the caller's own agent, stored under the shared agent's ID.
func suggestionOwnAgent() *types.CustomAgent {
	return &types.CustomAgent{ID: suggestionAgentID, TenantID: suggestionCallerTenant}
}

// unsharedSuggestionService is an agent service where the caller has no agent
// of this ID and the suggestion call itself succeeds.
func unsharedSuggestionService() *suggestionAgentService {
	return &suggestionAgentService{ownErr: service.ErrAgentNotFound, questions: suggestionSuggestions()}
}

func newSuggestionHandler(
	svc interfaces.CustomAgentService,
	shares interfaces.AgentShareService,
	disabled interfaces.TenantDisabledSharedAgentRepository,
	knowledge interfaces.KnowledgeRepository,
) *CustomAgentHandler {
	return &CustomAgentHandler{
		service:             svc,
		agentAccessResolver: service.NewAgentAccessResolver(svc, shares, disabled, knowledge),
	}
}

// suggestionRouter mounts the handler behind the identity auth attaches for a
// workspace member, on both surfaces a handler reads it from.
func suggestionRouter(h *CustomAgentHandler, user string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.ErrorHandler(), func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), suggestionCallerTenant)
		c.Set(types.UserIDContextKey.String(), user)
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, suggestionCallerTenant)
		ctx = types.WithCaller(ctx, types.Caller{
			TenantID: suggestionCallerTenant, UserID: user, Role: types.TenantRoleViewer,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.GET("/agents/:id/suggested-questions", h.GetSuggestedQuestions)
	return r
}

func getSuggestions(t *testing.T, r *gin.Engine, query string) (*httptest.ResponseRecorder, []types.SuggestedQuestion) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet, "/agents/"+suggestionAgentID+"/suggested-questions"+query, nil))
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var body struct {
		Data struct {
			Questions []types.SuggestedQuestion `json:"questions"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return rec, body.Data.Questions
}

// An own agent answers exactly as before the resolver existed: the request's
// knowledge range reaches the service untouched, in the caller's workspace.
func TestGetSuggestedQuestions_OwnAgentKeepsTheOriginalRequest(t *testing.T) {
	svc := &suggestionAgentService{
		own:       &types.CustomAgent{ID: suggestionAgentID, TenantID: suggestionCallerTenant},
		questions: suggestionSuggestions(),
	}
	shares := &suggestionAgentShares{agent: suggestionSharedAgent()}
	h := newSuggestionHandler(svc, shares, nil, nil)
	tags := []types.TagScope{{KnowledgeBaseID: "own-kb", TagIDs: []string{"tag-1"}}}

	rec, questions := getSuggestions(t, suggestionRouter(h, "member"),
		"?knowledge_base_ids=own-kb,own-kb-2&knowledge_ids=own-doc&limit=4&tag_scopes="+
			url.QueryEscape(`[{"knowledge_base_id":"own-kb","tag_ids":["tag-1"]}]`))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, suggestionSuggestions(), questions)
	require.Equal(t, 1, svc.calls)
	require.Equal(t, []string{"own-kb", "own-kb-2"}, svc.gotKBs)
	require.Equal(t, []string{"own-doc"}, svc.gotDocs)
	require.Equal(t, tags, svc.gotTags)
	require.Equal(t, 4, svc.gotLimit)
	require.Equal(t, suggestionCallerTenant, svc.gotTenant, "an own agent runs in the caller's workspace")
	require.Equal(t, suggestionCallerTenant, svc.gotCaller)
	require.False(t, svc.gotGrant, "no share grant rides along for an own agent")
	require.Zero(t, shares.calls, "a share is not read once the own agent is found")
}

// A share answers with its owner's workspace as the execution scope and the
// agent's grant attached, whether or not the frontend named the source.
func TestGetSuggestedQuestions_ResolvesSharedAgent(t *testing.T) {
	for _, tt := range []struct {
		name         string
		query        string
		own          *types.CustomAgent // nil = the caller has no agent of this ID
		wantOwnCalls int
		wantSource   uint64
	}{
		{
			name:         "source named, a same-ID own agent is not used",
			query:        "?source_tenant_id=84",
			own:          &types.CustomAgent{ID: suggestionAgentID, TenantID: suggestionCallerTenant},
			wantOwnCalls: 0,
			wantSource:   suggestionSourceTenant,
		},
		{
			name:         "no source, as an old frontend sends it",
			wantOwnCalls: 1,
			wantSource:   0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := unsharedSuggestionService()
			svc.own = tt.own
			shares := &suggestionAgentShares{agent: suggestionSharedAgent()}
			h := newSuggestionHandler(svc, shares, nil, nil)

			rec, questions := getSuggestions(t, suggestionRouter(h, "member"), tt.query)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, suggestionSuggestions(), questions)
			require.Equal(t, 1, svc.calls)
			require.Equal(t, suggestionSourceTenant, svc.gotTenant, "the share answers from its owner's workspace")
			require.Equal(t, suggestionCallerTenant, svc.gotCaller, "the caller stays the borrower")
			require.True(t, svc.gotGrant, "the agent's knowledge-base grant must reach the service")
			require.Equal(t, suggestionCallerTenant, shares.tenant)
			require.Equal(t, types.TenantRoleViewer, shares.role)
			require.Equal(t, tt.wantSource, shares.source)
			require.Equal(t, tt.wantOwnCalls, svc.ownCalls)
		})
	}
}

// An API key resolves a share exactly as a workspace member does. Its own
// knowledge-base scope, when it has one, is applied by the service, not here.
func TestGetSuggestedQuestions_APIKeyPrincipalUsesSharesLikeChat(t *testing.T) {
	svc := unsharedSuggestionService()
	shares := &suggestionAgentShares{agent: suggestionSharedAgent()}
	h := newSuggestionHandler(svc, shares, nil, nil)

	rec, questions := getSuggestions(t, suggestionRouter(h, "system-1"), "?source_tenant_id=84")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, suggestionSuggestions(), questions)
	require.Equal(t, 1, shares.calls)
}

// Unknown, unshared, revoked and locally disabled agents are all "not found",
// and no suggestion content leaves with the refusal.
func TestGetSuggestedQuestions_RefusesWhatTheCallerMayNotUse(t *testing.T) {
	own := &types.CustomAgent{ID: suggestionAgentID, TenantID: suggestionCallerTenant}
	disabledRecord := &types.TenantDisabledSharedAgent{
		TenantID: suggestionCallerTenant, AgentID: suggestionAgentID, SourceTenantID: suggestionSourceTenant,
	}
	for _, tt := range []struct {
		name     string
		query    string
		own      *types.CustomAgent // nil = the caller has no agent of this ID
		shareErr error
		disabled []*types.TenantDisabledSharedAgent
	}{
		{
			name:     "no such agent",
			shareErr: service.ErrAgentSharePermission,
		},
		{
			name:     "agent exists elsewhere but is not shared to this workspace",
			query:    "?source_tenant_id=84",
			shareErr: service.ErrAgentShareNotFound,
		},
		{
			name:     "own workspace named as the source",
			query:    "?source_tenant_id=1",
			own:      own,
			shareErr: service.ErrAgentShareNotFound,
		},
		{
			name:     "share revoked",
			query:    "?source_tenant_id=84",
			own:      own,
			shareErr: service.ErrAgentSharePermission,
		},
		{
			name:     "share revoked, no source",
			shareErr: service.ErrAgentSharePermission,
		},
		{
			name:     "share disabled in this workspace",
			query:    "?source_tenant_id=84",
			disabled: []*types.TenantDisabledSharedAgent{disabledRecord},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := unsharedSuggestionService()
			svc.own = tt.own
			shares := &suggestionAgentShares{agent: suggestionSharedAgent(), err: tt.shareErr}
			h := newSuggestionHandler(svc, shares, suggestionDisabledList{records: tt.disabled}, nil)

			rec, _ := getSuggestions(t, suggestionRouter(h, "member"), tt.query)

			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "questions")
			require.Zero(t, svc.calls, "a refused request never reaches the suggestion service")
		})
	}
}

// The request's knowledge range is narrowed to what the share lets this
// member read, and a range entirely outside it yields nothing at all rather
// than the agent's own knowledge bases.
func TestGetSuggestedQuestions_NarrowsTheRangeToTheShare(t *testing.T) {
	documents := map[string]*types.Knowledge{
		"doc-in":  {ID: "doc-in", TenantID: suggestionSourceTenant, KnowledgeBaseID: "kb-in"},
		"doc-out": {ID: "doc-out", TenantID: suggestionSourceTenant, KnowledgeBaseID: "kb-out"},
		"doc-own": {ID: "doc-own", TenantID: suggestionCallerTenant, KnowledgeBaseID: "own-kb"},
	}
	outsideTag := url.QueryEscape(`[{"knowledge_base_id":"kb-out","tag_ids":["tag-out"]}]`)
	bothTags := url.QueryEscape(`[{"knowledge_base_id":"kb-in","tag_ids":["tag-in"]},` +
		`{"knowledge_base_id":"kb-out","tag_ids":["tag-out"]}]`)
	for _, tt := range []struct {
		name      string
		query     string
		wantKBs   []string
		wantDocs  []string
		wantTags  []types.TagScope
		wantCalls int
	}{
		{
			name: "outside knowledge bases, documents and tags are dropped",
			query: "?source_tenant_id=84&knowledge_base_ids=kb-in,kb-out&knowledge_ids=doc-in,doc-out,doc-own" +
				"&tag_scopes=" + bothTags,
			wantKBs:   []string{"kb-in"},
			wantDocs:  []string{"doc-in"},
			wantTags:  []types.TagScope{{KnowledgeBaseID: "kb-in", TagIDs: []string{"tag-in"}}},
			wantCalls: 1,
		},
		{
			name:      "knowledge bases entirely outside the share give an empty list",
			query:     "?source_tenant_id=84&knowledge_base_ids=kb-out",
			wantCalls: 0,
		},
		{
			name:      "documents entirely outside the share give an empty list",
			query:     "?source_tenant_id=84&knowledge_ids=doc-out,doc-own",
			wantCalls: 0,
		},
		{
			name:      "tag scopes entirely outside the share give an empty list",
			query:     "?source_tenant_id=84&tag_scopes=" + outsideTag,
			wantCalls: 0,
		},
		{
			name:      "no range named keeps the agent configuration",
			query:     "?source_tenant_id=84",
			wantCalls: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := unsharedSuggestionService()
			h := newSuggestionHandler(svc, &suggestionAgentShares{agent: suggestionSharedAgent()}, nil,
				suggestionKnowledge{byID: documents})

			rec, questions := getSuggestions(t, suggestionRouter(h, "member"), tt.query)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tt.wantCalls, svc.calls)
			if tt.wantCalls == 1 {
				require.Equal(t, suggestionSuggestions(), questions)
				require.Equal(t, tt.wantKBs, svc.gotKBs)
				require.Equal(t, tt.wantDocs, svc.gotDocs)
				require.Equal(t, tt.wantTags, svc.gotTags)
				return
			}
			require.Empty(t, questions)
			require.JSONEq(t, `{"success":true,"data":{"questions":[]}}`, rec.Body.String(),
				"an empty list, never null and never the agent's own range")
		})
	}
}

func TestGetSuggestedQuestions_RejectsMalformedSourceTenantID(t *testing.T) {
	for _, raw := range []string{"abc", "84abc", "-1", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			svc := &suggestionAgentService{own: suggestionOwnAgent()}
			h := newSuggestionHandler(svc, nil, nil, nil)

			rec, _ := getSuggestions(t, suggestionRouter(h, "member"), "?source_tenant_id="+raw)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "source_tenant_id")
			require.Zero(t, svc.calls)
		})
	}
}

// A blank selector is an absent one.
func TestGetSuggestedQuestions_TreatsBlankSourceAsAbsent(t *testing.T) {
	svc := unsharedSuggestionService()
	shares := &suggestionAgentShares{agent: suggestionSharedAgent()}
	h := newSuggestionHandler(svc, shares, nil, nil)

	rec, questions := getSuggestions(t, suggestionRouter(h, "member"), "?source_tenant_id=%20")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, suggestionSuggestions(), questions)
	require.Equal(t, uint64(0), shares.source)
}

// A failing lookup is a server error, not "not found", and the text of a raw
// store error never reaches the client.
func TestGetSuggestedQuestions_ReportsLookupFailuresAsServerErrors(t *testing.T) {
	for _, tt := range []struct {
		name  string
		query string
		h     *CustomAgentHandler
	}{
		{
			name: "resolver not configured",
			h:    &CustomAgentHandler{service: &suggestionAgentService{}},
		},
		{
			name: "own agent lookup failing",
			h: newSuggestionHandler(
				&suggestionAgentService{ownErr: errors.New("db down: password=hunter2")},
				&suggestionAgentShares{err: service.ErrAgentSharePermission}, nil, nil,
			),
		},
		{
			name:  "share lookup failing",
			query: "?source_tenant_id=84",
			h: newSuggestionHandler(
				&suggestionAgentService{ownErr: service.ErrAgentNotFound},
				&suggestionAgentShares{err: errors.New("share store down: password=hunter2")}, nil, nil,
			),
		},
		{
			name:  "disabled list failing",
			query: "?source_tenant_id=84",
			h: newSuggestionHandler(
				&suggestionAgentService{ownErr: service.ErrAgentNotFound, questions: suggestionSuggestions()},
				&suggestionAgentShares{agent: suggestionSharedAgent()},
				suggestionDisabledList{err: errors.New("disabled list: password=hunter2")},
				nil,
			),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := getSuggestions(t, suggestionRouter(tt.h, "member"), tt.query)

			require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "hunter2")
			require.NotContains(t, rec.Body.String(), "leave policy")
		})
	}
}
