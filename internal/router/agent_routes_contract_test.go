package router

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/application/service"
	"github.com/primind/primind/internal/handler"
	"github.com/primind/primind/internal/middleware"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

// agentRouteClass is how a route under /agents/:id treats an agent the caller
// may only reach through a share from another workspace.
type agentRouteClass string

const (
	// agentReadShared answers for a shared agent too. The contract test runs
	// each such route as the owner, as a share's member, without a share, and
	// after the share is revoked.
	agentReadShared agentRouteClass = "读取（需支持共享）"
	// agentReadLocal reads only the caller's own agents. Borrowers get a shared
	// agent's details from the share listing, never from here.
	agentReadLocal agentRouteClass = "读取（仅本空间）"
	// agentWriteLocal creates or changes something in the caller's own workspace.
	// A shared agent is read-only for its borrowers and is never written through.
	agentWriteLocal agentRouteClass = "写入（仅本空间）"
)

// agentRouteContract lists every registered route whose path addresses an
// agent by ID. A route under /agents/:id that is missing here fails
// TestAgentRoutesAreListedInContract: add it with its class, and make it
// resolve the agent through service.AgentAccessResolver.
var agentRouteContract = map[string]agentRouteClass{
	http.MethodGet + " /api/v1/agents/:id":                     agentReadLocal,
	http.MethodPut + " /api/v1/agents/:id":                     agentWriteLocal,
	http.MethodDelete + " /api/v1/agents/:id":                  agentWriteLocal,
	http.MethodPost + " /api/v1/agents/:id/copy":               agentWriteLocal,
	http.MethodGet + " /api/v1/agents/:id/suggested-questions": agentReadShared,
	http.MethodPost + " /api/v1/agents/:id/shares":             agentWriteLocal,
	http.MethodGet + " /api/v1/agents/:id/shares":              agentReadLocal,
	http.MethodDelete + " /api/v1/agents/:id/shares/:share_id": agentWriteLocal,
	http.MethodPost + " /api/v1/agents/:id/embed-channels":     agentWriteLocal,
	http.MethodGet + " /api/v1/agents/:id/embed-channels":      agentReadLocal,
	http.MethodPost + " /api/v1/agents/:id/im-channels":        agentWriteLocal,
	http.MethodGet + " /api/v1/agents/:id/im-channels":         agentReadLocal,
}

// agentRouteRegistrars are every function of this package that registers a
// route under /agents. TestAgentRouteRegistrarsAreKnown compares it with the
// package source, so a route added in a new function cannot bypass the engine
// the contract test enumerates.
var agentRouteRegistrars = map[string]bool{
	"RegisterCustomAgentRoutes":  true,
	"RegisterOrganizationRoutes": true,
	"RegisterEmbedChannelRoutes": true,
	"RegisterIMChannelRoutes":    true,
}

// agentRoutesEngine registers the agent routes in NewRouter's order.
func agentRoutesEngine(agents *handler.CustomAgentHandler, middleware ...gin.HandlerFunc) *gin.Engine {
	engine := gin.New()
	engine.Use(middleware...)
	v1 := engine.Group("/api/v1")
	g := &rbacGuards{}
	RegisterCustomAgentRoutes(v1, agents, g)
	RegisterOrganizationRoutes(v1, &handler.OrganizationHandler{}, g)
	RegisterIMChannelRoutes(v1, &handler.IMHandler{}, g)
	RegisterEmbedChannelRoutes(v1, &handler.EmbedChannelHandler{}, g)
	return engine
}

func TestAgentRoutesAreListedInContract(t *testing.T) {
	gin.SetMode(gin.TestMode)

	routes := agentRoutesEngine(&handler.CustomAgentHandler{}).Routes()

	registered := make(map[string]bool)
	for _, route := range routes {
		if !strings.Contains(route.Path, "/agents/:id") {
			continue
		}
		key := route.Method + " " + route.Path
		registered[key] = true
		if _, listed := agentRouteContract[key]; !listed {
			t.Errorf("%s is not listed in agentRouteContract: add it with its class "+
				"(读取（需支持共享）/ 读取（仅本空间）/ 写入（仅本空间）)", key)
		}
	}
	for key := range agentRouteContract {
		if !registered[key] {
			t.Errorf("%s is listed in agentRouteContract but is no longer registered: remove the entry", key)
		}
	}
}

func TestAgentRouteRegistrarsAreKnown(t *testing.T) {
	found := make(map[string]string)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !registersUnderAgents(fn) {
				continue
			}
			found[fn.Name.Name] = file
		}
	}
	for name, file := range found {
		if !agentRouteRegistrars[name] {
			t.Errorf("%s (%s) registers /agents routes but is not part of agentRoutesEngine; "+
				"add it there and to agentRouteRegistrars", name, file)
		}
	}
	for name := range agentRouteRegistrars {
		if _, ok := found[name]; !ok {
			t.Errorf("%s no longer registers /agents routes: drop it from agentRouteRegistrars", name)
		}
	}
}

// registersUnderAgents reports whether fn holds a string literal path under
// /agents, such as a group base path or a route path.
func registersUnderAgents(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if found {
			return false
		}
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && strings.HasPrefix(value, "/agents") {
			found = true
			return false
		}
		return true
	})
	return found
}

const (
	contractCallerTenant = uint64(7)
	contractSourceTenant = uint64(84)
	contractAgentID      = "contract-agent"
	contractQuestion     = "What is the leave policy?"
)

// contractAgentService backs both lookups a read route makes: the agent
// resolution and the read itself.
type contractAgentService struct {
	interfaces.CustomAgentService
	own       *types.CustomAgent
	ownErr    error
	questions []types.SuggestedQuestion
}

func (s *contractAgentService) GetAgentByID(context.Context, string) (*types.CustomAgent, error) {
	return s.own, s.ownErr
}

func (s *contractAgentService) GetSuggestedQuestions(
	context.Context, string, []string, []string, []types.TagScope, int,
) ([]types.SuggestedQuestion, error) {
	return s.questions, nil
}

// contractAgentShares offers one share, or refuses it the way the share
// service does when there is no share and when one was revoked.
type contractAgentShares struct {
	interfaces.AgentShareService
	shared *types.CustomAgent
	refuse error // nil = the share is offered
}

func (s *contractAgentShares) GetSharedAgentForTenant(
	context.Context, uint64, types.TenantRole, string, ...uint64,
) (*types.CustomAgent, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	return s.shared, nil
}

type contractEngine struct {
	engine *gin.Engine
	agents *contractAgentService
	shares *contractAgentShares
}

// newContractEngine mounts the real agent routes behind the ErrorHandler and
// the identity auth attaches for a workspace member.
func newContractEngine() *contractEngine {
	e := &contractEngine{
		agents: &contractAgentService{
			ownErr:    service.ErrAgentNotFound,
			questions: []types.SuggestedQuestion{{Question: contractQuestion, Source: "agent_config"}},
		},
		shares: &contractAgentShares{shared: &types.CustomAgent{
			ID: contractAgentID, TenantID: contractSourceTenant,
			Config: types.CustomAgentConfig{KBSelectionMode: "selected", KnowledgeBases: []string{"kb-in"}},
		}},
	}
	resolver := service.NewAgentAccessResolver(e.agents, e.shares, nil, nil)
	agents := handler.NewCustomAgentHandler(
		e.agents, nil, nil, nil, nil, service.HostSandboxManager{}, resolver)
	e.engine = agentRoutesEngine(agents, middleware.ErrorHandler(), func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), contractCallerTenant)
		c.Set(types.UserIDContextKey.String(), "member")
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, contractCallerTenant)
		ctx = types.WithCaller(ctx, types.Caller{
			TenantID: contractCallerTenant, UserID: "member", Role: types.TenantRoleViewer,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	return e
}

func (e *contractEngine) get(method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.engine.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// Every route that must support shared agents answers the owner and the
// share's member, and refuses a caller without a share and one whose share was
// revoked, with no suggestion content in the refusal.
func TestAgentRoutesThatSupportSharedAgents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for key, class := range agentRouteContract {
		if class != agentReadShared {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		path = strings.ReplaceAll(path, ":id", contractAgentID)

		t.Run(key, func(t *testing.T) {
			e := newContractEngine()

			// The owner's own agent.
			e.agents.own = &types.CustomAgent{ID: contractAgentID, TenantID: contractCallerTenant}
			rec := e.get(method, path)
			require.Equal(t, http.StatusOK, rec.Code, "own agent: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), contractQuestion)

			// A member of the workspace the agent is shared to.
			e.agents.own, e.agents.ownErr = nil, service.ErrAgentNotFound
			rec = e.get(method, path+"?agent_source_tenant_id=84")
			require.Equal(t, http.StatusOK, rec.Code, "shared member: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), contractQuestion)

			// No share relation.
			e.shares.refuse = service.ErrAgentShareNotFound
			rec = e.get(method, path+"?agent_source_tenant_id=84")
			require.Equal(t, http.StatusNotFound, rec.Code, "no share: %s", rec.Body.String())
			require.NotContains(t, rec.Body.String(), contractQuestion)

			// The share is revoked after the member had access.
			e.shares.refuse = nil
			require.Equal(t, http.StatusOK, e.get(method, path+"?agent_source_tenant_id=84").Code)
			e.shares.refuse = service.ErrAgentSharePermission
			rec = e.get(method, path+"?agent_source_tenant_id=84")
			require.Equal(t, http.StatusNotFound, rec.Code, "revoked: %s", rec.Body.String())
			require.NotContains(t, rec.Body.String(), contractQuestion)
		})
	}
}

// A read that is local-only does not hand out another workspace's agent: its
// configuration carries prompts and model bindings that a share withholds.
func TestAgentDetailRouteDoesNotServeSharedAgents(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := newContractEngine()

	rec := e.get(http.MethodGet, "/api/v1/agents/"+contractAgentID+"?agent_source_tenant_id=84")

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), contractAgentID, "the response must not carry the shared agent")
}
