package middleware

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/types"
	"github.com/sirupsen/logrus"
)

// testInviteCode has the shape of a real invite code: 16 hex characters.
const (
	testInviteCode       = "6e9b1c2d3e4f5a6b"
	testInviteCodeMasked = "6e9b************"
)

// serveWithAccessLog runs one request through Logger() on a real gin engine and
// returns what the access log wrote. The handler echoes the invite code back,
// so the response body is exercised as well as the path and request body.
func serveWithAccessLog(t *testing.T, method, path, body string) string {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var out bytes.Buffer
	base := logrus.New()
	base.SetOutput(&out)
	base.SetFormatter(&logrus.TextFormatter{DisableColors: true, DisableTimestamp: true})
	entry := logrus.NewEntry(base)

	r := gin.New()
	// Production (router.NewRouter) turns this on, which is what lets
	// logger.GetLogger(c) find the entry stored on the request context.
	r.ContextWithFallback = true
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), types.LoggerContextKey, entry))
		c.Next()
	})
	r.Use(Logger())
	echo := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"invite_code": testInviteCode, "name": "研发共享空间"}})
	}
	r.GET("/api/v1/organizations/preview/:code", echo)
	r.POST("/api/v1/organizations/join", echo)
	r.POST("/api/v1/organizations/join-request", echo)
	r.POST("/api/v1/organizations/join-by-id", echo)
	r.GET("/api/v1/organizations/:id", echo)

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	r.ServeHTTP(httptest.NewRecorder(), req)

	if out.Len() == 0 {
		t.Fatalf("%s %s wrote no access log line", method, path)
	}
	return out.String()
}

// The three invite-code routes still get an access log line, but the code in
// the path, request body and response body keeps only its first four characters.
func TestLoggerMasksInviteCodeOnInviteRoutes(t *testing.T) {
	cases := []struct {
		name, method, path, body string
	}{
		{"preview", http.MethodGet, "/api/v1/organizations/preview/" + testInviteCode, ""},
		{"join", http.MethodPost, "/api/v1/organizations/join", `{"invite_code":"` + testInviteCode + `"}`},
		{
			"join-request", http.MethodPost, "/api/v1/organizations/join-request",
			`{"invite_code":"` + testInviteCode + `","message":"请通过","role":"viewer"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := serveWithAccessLog(t, tc.method, tc.path, tc.body)

			if strings.Contains(line, testInviteCode) {
				t.Fatalf("access log leaked the full invite code:\n%s", line)
			}
			if !strings.Contains(line, testInviteCodeMasked) {
				t.Fatalf("access log lacks the masked code %q:\n%s", testInviteCodeMasked, line)
			}
			if !strings.Contains(line, "status_code=200") {
				t.Fatalf("access log line lost its metadata:\n%s", line)
			}
			if tc.method == http.MethodGet {
				if !strings.Contains(line, "/api/v1/organizations/preview/"+testInviteCodeMasked) {
					t.Fatalf("preview path not masked as expected:\n%s", line)
				}
			} else {
				if !strings.Contains(line, "path="+tc.path) {
					t.Fatalf("join path should be logged unchanged:\n%s", line)
				}
				if !strings.Contains(line, "request_body=") {
					t.Fatalf("request body dropped from the access log:\n%s", line)
				}
			}
			if tc.name == "join-request" && !strings.Contains(line, "请通过") {
				t.Fatalf("other request fields should survive masking:\n%s", line)
			}
		})
	}
}

// Routes that are not invite-code routes keep logging exactly what they did.
func TestLoggerLeavesOtherRoutesUnmasked(t *testing.T) {
	cases := []struct {
		name, method, path, body string
	}{
		{"join-by-id", http.MethodPost, "/api/v1/organizations/join-by-id", `{"invite_code":"` + testInviteCode + `"}`},
		{"organization detail", http.MethodGet, "/api/v1/organizations/" + testInviteCode, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := serveWithAccessLog(t, tc.method, tc.path, tc.body)
			if strings.Contains(line, testInviteCodeMasked) {
				t.Fatalf("masking leaked onto a non-invite route:\n%s", line)
			}
			if !strings.Contains(line, "path="+tc.path) {
				t.Fatalf("path changed on a non-invite route:\n%s", line)
			}
		})
	}
}

func TestMaskInviteCode(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"abc":               "***",
		"abcd":              "****",
		"abcde":             "abcd*",
		testInviteCode:      testInviteCodeMasked,
		"0123456789abcdef0": "0123*************",
	}
	for in, want := range cases {
		if got := maskInviteCode(in); got != want {
			t.Errorf("maskInviteCode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskInviteCodeInBody(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", `{"invite_code":"6e9b1c2d3e4f5a6b"}`, `{"invite_code":"6e9b************"}`},
		{"spaced and upper-case key", `{ "INVITE_CODE" : "6e9b1c2d" }`, `{ "INVITE_CODE" : "6e9b****" }`},
		{"escaped quote stays inside the value", `{"invite_code":"6e9b\"xyz"}`, `{"invite_code":"6e9b*****"}`},
		{
			"other fields untouched",
			`{"message":"hi","invite_code_validity_days":7}`,
			`{"message":"hi","invite_code_validity_days":7}`,
		},
	}
	for _, tc := range cases {
		if got := maskInviteCodeInBody(tc.in); got != tc.want {
			t.Errorf("%s: maskInviteCodeInBody(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestMaskInviteCodeInPath(t *testing.T) {
	const preview = "/api/v1/organizations/preview/"
	cases := map[string]string{
		preview + testInviteCode:                  preview + testInviteCodeMasked,
		preview + testInviteCode + "/x":           preview + testInviteCodeMasked + "/x",
		preview:                                   preview,
		"/api/v1/organizations/" + testInviteCode: "/api/v1/organizations/" + testInviteCode,
	}
	for in, want := range cases {
		if got := maskInviteCodeInPath(in); got != want {
			t.Errorf("maskInviteCodeInPath(%q) = %q, want %q", in, got, want)
		}
	}
}
