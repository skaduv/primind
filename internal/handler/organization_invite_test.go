package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/application/repository"
	"github.com/primind/primind/internal/application/service"
	apperrors "github.com/primind/primind/internal/errors"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type inviteRepOrg struct {
	interfaces.OrganizationService
	members        []*types.OrganizationTenantMember
	representative *string
}

func (*inviteRepOrg) IsTenantOrgAdmin(context.Context, string, uint64) (bool, error) {
	return true, nil
}

// GetTenantMember reports a caller that is a member of the org when the
// roster is set, and a not-yet-member invite target otherwise.
func (o *inviteRepOrg) GetTenantMember(
	_ context.Context, _ string, tenantID uint64,
) (*types.OrganizationTenantMember, error) {
	for _, member := range o.members {
		if member.TenantID == tenantID {
			return member, nil
		}
	}
	return nil, repository.ErrOrgMemberNotFound
}

func (o *inviteRepOrg) AddTenantMember(_ context.Context, _ string, _ uint64, rep string, _ types.OrgMemberRole) error {
	o.representative = &rep
	return nil
}

func (o *inviteRepOrg) ListTenantMembers(context.Context, string) ([]*types.OrganizationTenantMember, error) {
	return o.members, nil
}

type inviteRepTenants struct{ interfaces.TenantService }

func (inviteRepTenants) GetTenantByID(_ context.Context, id uint64) (*types.Tenant, error) {
	return &types.Tenant{ID: id}, nil
}

func (inviteRepTenants) GetTenantsByIDs(_ context.Context, ids []uint64) (map[uint64]*types.Tenant, error) {
	out := map[uint64]*types.Tenant{}
	for _, id := range ids {
		out[id] = &types.Tenant{ID: id}
	}
	return out, nil
}

type inviteRepUsers struct{ interfaces.UserService }

func (inviteRepUsers) GetUserByID(_ context.Context, id string) (*types.User, error) {
	return &types.User{ID: id, TenantID: 42}, nil
}

func inviteRepContext(body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: "org"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(types.TenantIDContextKey.String(), uint64(7))
	return c, recorder
}

// A direct add must not let the inviter attach a user of the other workspace
// as its representative: the roster would then show that user to members.
func TestInviteMemberAttachesNoRepresentative(t *testing.T) {
	for _, body := range []string{
		`{"tenant_id":42,"representative_user_id":"victim","role":"viewer"}`,
		`{"user_id":"victim","role":"viewer"}`,
	} {
		org := &inviteRepOrg{}
		h := &OrganizationHandler{orgService: org, tenantService: inviteRepTenants{}, userService: inviteRepUsers{}}
		c, _ := inviteRepContext(body)
		h.InviteMember(c)
		require.Empty(t, c.Errors, body)
		require.NotNil(t, org.representative, body)
		require.Empty(t, *org.representative, body)
	}
}

// Other workspaces' users' emails are not part of the roster.
func TestListMembersShowsEmailOnlyForOwnWorkspace(t *testing.T) {
	org := &inviteRepOrg{members: []*types.OrganizationTenantMember{
		{ID: "m-own", TenantID: 7, RepresentativeUser: &types.User{Username: "me", Email: "me@own.example"}},
		{ID: "m-other", TenantID: 42, RepresentativeUser: &types.User{Username: "them", Email: "them@other.example"}},
	}}
	h := &OrganizationHandler{orgService: org, tenantService: inviteRepTenants{}}
	c, recorder := inviteRepContext("")
	h.ListMembers(c)
	require.Empty(t, c.Errors)
	require.Contains(t, recorder.Body.String(), "me@own.example")
	require.NotContains(t, recorder.Body.String(), "them@other.example")
	require.Contains(t, recorder.Body.String(), `"username":"them"`)
}

type scopedListKBShares struct{ interfaces.KBShareService }

func (scopedListKBShares) ListSharedKnowledgeBases(
	context.Context, uint64, types.TenantRole,
) ([]*types.SharedKnowledgeBaseInfo, error) {
	return []*types.SharedKnowledgeBaseInfo{
		{KnowledgeBase: &types.KnowledgeBase{ID: "kb-allowed", Name: "Allowed KB"}},
		{KnowledgeBase: &types.KnowledgeBase{ID: "kb-other", Name: "Other KB"}},
	}, nil
}

// A KB-restricted API key must not learn about shared KBs outside its
// allow-list from the organization listings.
func TestSharedKnowledgeBaseListRespectsAPIKeyScope(t *testing.T) {
	h := &OrganizationHandler{shareService: scopedListKBShares{}}
	c, recorder := inviteRepContext("")
	ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(7))
	ctx = types.WithTenantAPIKeyScope(ctx, types.TenantAPIKeyScope{
		KnowledgeBaseIDs: types.StringArray{"kb-allowed"},
		Capabilities:     types.StringArray{string(types.APIKeyCapabilityManageSpaces)},
	})
	c.Request = c.Request.WithContext(ctx)

	h.ListSharedKnowledgeBases(c)
	require.Empty(t, c.Errors)
	require.Contains(t, recorder.Body.String(), "Allowed KB")
	require.NotContains(t, recorder.Body.String(), "Other KB")
}

// ---- Invite-code lookup failures (BF-06) ----

const testInviteCode = "6e9b738f4f62c1e0"

// inviteLookupOrg scripts the invite-code entry points: lookups and joins
// fail with the configured error, or return the fixed space.
type inviteLookupOrg struct {
	interfaces.OrganizationService
	org       *types.Organization
	lookupErr error
	joinErr   error
}

func (o *inviteLookupOrg) GetOrganizationByInviteCode(context.Context, string) (*types.Organization, error) {
	if o.lookupErr != nil {
		return nil, o.lookupErr
	}
	return o.org, nil
}

func (o *inviteLookupOrg) JoinByInviteCode(context.Context, string, string, uint64) (*types.Organization, error) {
	if o.joinErr != nil {
		return nil, o.joinErr
	}
	return o.org, nil
}

// inviteErrorCapture renders c.Errors the way middleware.ErrorHandler
// renders an AppError, so the tests assert on the wire format clients see.
// The tenant is fixed so the log field can be checked too.
func inviteErrorCapture() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(types.TenantIDContextKey.String(), uint64(7))
		c.Next()
		if len(c.Errors) == 0 {
			return
		}
		if appErr, ok := c.Errors.Last().Err.(*apperrors.AppError); ok {
			c.JSON(appErr.HTTPCode, gin.H{"success": false, "error": appErr})
		}
	}
}

type inviteCall struct {
	method string
	path   string
	body   string
}

func previewInviteCall() inviteCall {
	return inviteCall{method: http.MethodGet, path: "/organizations/preview/" + testInviteCode}
}

func joinInviteCall() inviteCall {
	body := `{"invite_code":"` + testInviteCode + `"}`
	return inviteCall{method: http.MethodPost, path: "/organizations/join", body: body}
}

func joinRequestInviteCall() inviteCall {
	body := `{"invite_code":"` + testInviteCode + `"}`
	return inviteCall{method: http.MethodPost, path: "/organizations/join-request", body: body}
}

type inviteAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// serveInvite runs one request against the three invite endpoints wired to
// org and returns the status, the decoded error body and what was logged.
func serveInvite(t *testing.T, org *inviteLookupOrg, call inviteCall) (int, inviteAPIError, string) {
	t.Helper()
	var logs bytes.Buffer
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(os.Stdout) })

	gin.SetMode(gin.TestMode)
	h := &OrganizationHandler{orgService: org}
	r := gin.New()
	r.Use(inviteErrorCapture())
	r.GET("/organizations/preview/:code", h.PreviewByInviteCode)
	r.POST("/organizations/join", h.JoinByInviteCode)
	r.POST("/organizations/join-request", h.SubmitJoinRequest)

	req := httptest.NewRequest(call.method, call.path, strings.NewReader(call.body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var payload struct {
		Error inviteAPIError `json:"error"`
	}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode response %q: %v", w.Body.String(), err)
		}
	}
	return w.Code, payload.Error, logs.String()
}

// inviteLookupFailure is one service-layer outcome of an invite-code lookup
// and the response and log line the handler must produce for it.
type inviteLookupFailure struct {
	name        string
	err         error
	wantStatus  int
	wantCode    int
	wantMessage string
	wantReason  string
	wantLevel   string
}

var inviteLookupFailures = []inviteLookupFailure{
	{
		name: "not found", err: service.ErrOrgNotFound,
		wantStatus: http.StatusNotFound, wantCode: int(apperrors.ErrInviteCodeNotFound),
		wantMessage: "Invalid invite code", wantReason: "not_found", wantLevel: "WARNING",
	},
	{
		name: "expired", err: service.ErrInviteCodeExpired,
		wantStatus: http.StatusNotFound, wantCode: int(apperrors.ErrInviteCodeExpired),
		wantMessage: "Invalid invite code", wantReason: "expired", wantLevel: "WARNING",
	},
	{
		name: "internal", err: errors.New("pq: connection refused"),
		wantStatus: http.StatusInternalServerError, wantCode: int(apperrors.ErrInternalServer),
		wantMessage: "Failed to look up invite code", wantReason: "internal", wantLevel: "ERROR",
	},
}

func assertInviteLookupFailure(t *testing.T, status int, body inviteAPIError, logs, op string, tc inviteLookupFailure) {
	t.Helper()
	if status != tc.wantStatus {
		t.Errorf("status = %d, want %d", status, tc.wantStatus)
	}
	if body.Code != tc.wantCode {
		t.Errorf("error.code = %d, want %d", body.Code, tc.wantCode)
	}
	if body.Message != tc.wantMessage {
		t.Errorf("error.message = %q, want %q", body.Message, tc.wantMessage)
	}

	line := ""
	for _, candidate := range strings.Split(logs, "\n") {
		if strings.Contains(candidate, "reason=") {
			line = candidate
			break
		}
	}
	if !strings.HasPrefix(line, tc.wantLevel) {
		t.Errorf("log level = %q, want %s: %q", line, tc.wantLevel, logs)
	}
	wantFields := []string{"op=" + op, "reason=" + tc.wantReason, "tenant_id=7", "invite_code=6e9b************"}
	for _, field := range wantFields {
		if !strings.Contains(line, field) {
			t.Errorf("log line lacks %q: %q", field, line)
		}
	}
	if strings.Contains(logs, testInviteCode) {
		t.Errorf("log leaks the full invite code: %q", logs)
	}
	if tc.wantLevel == "ERROR" && !strings.Contains(line, "error=pq: connection refused") {
		t.Errorf("internal failure must log the original error: %q", line)
	}
}

func TestPreviewByInviteCodeLookupFailures(t *testing.T) {
	for _, tc := range inviteLookupFailures {
		t.Run(tc.name, func(t *testing.T) {
			status, body, logs := serveInvite(t, &inviteLookupOrg{lookupErr: tc.err}, previewInviteCall())
			assertInviteLookupFailure(t, status, body, logs, "preview", tc)
		})
	}
}

func TestSubmitJoinRequestInviteLookupFailures(t *testing.T) {
	for _, tc := range inviteLookupFailures {
		t.Run(tc.name, func(t *testing.T) {
			status, body, logs := serveInvite(t, &inviteLookupOrg{lookupErr: tc.err}, joinRequestInviteCall())
			assertInviteLookupFailure(t, status, body, logs, "join_request", tc)
		})
	}
}

func TestJoinByInviteCodeLookupFailures(t *testing.T) {
	for _, tc := range inviteLookupFailures {
		t.Run(tc.name, func(t *testing.T) {
			// The join service call also inserts the membership, so a 500
			// names the whole join instead of blaming the lookup.
			if tc.wantReason == "internal" {
				tc.wantMessage = "Failed to join organization"
			}
			status, body, logs := serveInvite(t, &inviteLookupOrg{joinErr: tc.err}, joinInviteCall())
			assertInviteLookupFailure(t, status, body, logs, "join", tc)
		})
	}
}

// A space that admits members only through review used to answer a direct
// join with "Invalid invite code". The code was fine; the caller picked the
// wrong endpoint, and the message must now say so, mirroring what the
// join-request endpoint says about a space that does NOT need approval.
func TestJoinEndpointsMirrorEachOthersApprovalGuidance(t *testing.T) {
	status, body, logs := serveInvite(t, &inviteLookupOrg{joinErr: service.ErrOrgPermissionDenied}, joinInviteCall())
	if status != http.StatusBadRequest {
		t.Errorf("direct join on an approval space: status = %d, want 400", status)
	}
	if body.Message != "This organization requires approval. Use the join-request endpoint instead." {
		t.Errorf("direct join on an approval space: message = %q", body.Message)
	}
	if strings.Contains(logs, "reason=") {
		t.Errorf("a valid invite code must not be logged as a lookup failure: %q", logs)
	}

	openSpace := &types.Organization{ID: "org", RequireApproval: false}
	status, body, _ = serveInvite(t, &inviteLookupOrg{org: openSpace}, joinRequestInviteCall())
	if status != http.StatusBadRequest {
		t.Errorf("join request on an open space: status = %d, want 400", status)
	}
	if body.Message != "This organization does not require approval. Use the join endpoint instead." {
		t.Errorf("join request on an open space: message = %q", body.Message)
	}
}

func TestJoinByInviteCodeMemberLimitUnchanged(t *testing.T) {
	status, body, logs := serveInvite(t, &inviteLookupOrg{joinErr: service.ErrOrgMemberLimitReached}, joinInviteCall())
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if body.Code != int(apperrors.ErrValidation) {
		t.Errorf("error.code = %d, want the validation code %d", body.Code, apperrors.ErrValidation)
	}
	if body.Message != "该空间成员已满，无法加入" {
		t.Errorf("error.message = %q", body.Message)
	}
	if strings.Contains(logs, "reason=") {
		t.Errorf("a full space is not an invite lookup failure: %q", logs)
	}
}

func TestMaskInviteCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"16-char code keeps the first 4", "6e9b738f4f62c1e0", "6e9b************"},
		{"empty", "", ""},
		{"at the prefix length is masked entirely", "6e9b", "****"},
		{"below the prefix length is masked entirely", "6e", "**"},
		{"one past the prefix keeps one", "6e9b7", "6e9b*"},
		{"control character in the kept prefix is sanitized", "\nbc7738f4f62c1e0", " bc7************"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskInviteCode(tc.in); got != tc.want {
				t.Errorf("maskInviteCode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
