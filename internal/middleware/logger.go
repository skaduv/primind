package middleware

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/types"
	secutils "github.com/primind/primind/internal/utils"
)

const (
	maxBodySize = 1024 * 10 // 最大记录10KB的body内容
	// bodyTruncatedMarker 是日志里跟在被截断的 body 后面的标记。
	bodyTruncatedMarker = "... [内容过长，已截断]"
)

// replayedBody 把日志窥探消费掉的字节还给 handler，让它读到的 body 与没有这层
// 中间件时完全一致；Close 仍然关闭原始 body，连接得以释放。
type replayedBody struct {
	io.Reader
	io.Closer
}

// loggerResponseBodyWriter 自定义ResponseWriter用于捕获响应内容（用于logger中间件）
type loggerResponseBodyWriter struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

// Write 重写Write方法，同时写入buffer和原始writer
// 限制buffer大小，避免SSE等流式响应导致内存无限增长
func (r loggerResponseBodyWriter) Write(b []byte) (int, error) {
	if r.body.Len() < maxBodySize {
		remaining := maxBodySize - r.body.Len()
		if len(b) <= remaining {
			r.body.Write(b)
		} else {
			r.body.Write(b[:remaining])
		}
	}
	return r.ResponseWriter.Write(b)
}

// sensitiveFieldRegex 匹配 JSON 中的敏感字段（不区分大小写，兼容 snake_case / camelCase / PascalCase）。
// $1 捕获原始字段名（包括两侧引号），保持日志中的字段名不变，仅将值替换为 "***"。
var sensitiveFieldRegex = regexp.MustCompile(
	`(?i)("(?:new[_-]?password|old[_-]?password|password|passwd|ticket|token|access[_-]?token|` +
		`refresh[_-]?token|next[_-]?token|device[_-]?token|pending[_-]?token|pairing[_-]?link|` +
		`id[_-]?token|authorization|auth[_-]?token|api[_-]?key|` +
		`api[_-]?secret|secret[_-]?key|client[_-]?secret|private[_-]?key|secret|` +
		`authorization[_-]?url|authorization[_-]?attempt|assertion)")\s*:\s*"[^"]*"`,
)

// sanitizeBody 清理敏感信息
func sanitizeBody(body string) string {
	return sensitiveFieldRegex.ReplaceAllString(body, `$1:"***"`)
}

var sensitiveQueryFields = map[string]struct{}{
	"access_token":          {},
	"authorization_attempt": {},
	"code":                  {},
	"id_token":              {},
	"refresh_token":         {},
	"state":                 {},
	// ticket is the sandbox terminal's WebSocket handshake credential. A
	// browser cannot set Authorization on an upgrade, so it travels in the
	// query string; anyone holding it for its 2-minute TTL can open a shell
	// in the session's sandbox, which is why it must never reach a log line.
	"ticket": {},
	"token":  {},
}

// sanitizeQuery prevents OAuth authorization codes, CSRF/attempt state, and
// handshake credentials from being copied into access logs. Parsing the query
// also covers repeated and percent-encoded parameters without relying on
// fragile string replacement.
func sanitizeQuery(raw string) string {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "[invalid query omitted]"
	}
	for key := range values {
		if _, sensitive := sensitiveQueryFields[strings.ToLower(key)]; sensitive {
			values[key] = []string{"***"}
		}
	}
	return values.Encode()
}

// inviteCodeLogPrefixLen 是日志里一个邀请码最多保留的前缀长度。4 位十六进制
// 足够让管理员把日志行和自己发出的码对上，又短到无法拿来重放加入空间。
// 与 internal/handler/organization.go（BF-06）里 maskInviteCode 的规则一致。
const inviteCodeLogPrefixLen = 4

const (
	// inviteCodePreviewPathPrefix 之后的那一段路径就是邀请码（GET /preview/:code）。
	inviteCodePreviewPathPrefix = "/api/v1/organizations/preview/"
	inviteCodeJoinPath          = "/api/v1/organizations/join"
	inviteCodeJoinRequestPath   = "/api/v1/organizations/join-request"
)

// inviteCodeFieldRegex 匹配 JSON 里的 "invite_code":"..." 。字段名不区分大小写，
// 因为 encoding/json 绑定字段时也不区分；值允许出现转义引号，避免被 \" 截断后漏掉后半段。
var inviteCodeFieldRegex = regexp.MustCompile(`(?i)("invite_code"\s*:\s*")((?:[^"\\]|\\.)*)"`)

// maskInviteCode 保留邀请码前 inviteCodeLogPrefixLen 个字符，其余每个替换为 "*"；
// 长度不超过前缀的整体打码。最终写日志时 Logger 还会对整个字段做 SanitizeForLog。
func maskInviteCode(inviteCode string) string {
	if len(inviteCode) <= inviteCodeLogPrefixLen {
		return strings.Repeat("*", len(inviteCode))
	}
	return inviteCode[:inviteCodeLogPrefixLen] + strings.Repeat("*", len(inviteCode)-inviteCodeLogPrefixLen)
}

// maskInviteCodeInPath 把 /api/v1/organizations/preview/<code> 里的邀请码打码，
// 其他路径原样返回。
func maskInviteCodeInPath(path string) string {
	rest, ok := strings.CutPrefix(path, inviteCodePreviewPathPrefix)
	if !ok || rest == "" {
		return path
	}
	code, tail, hasTail := strings.Cut(rest, "/")
	masked := inviteCodePreviewPathPrefix + maskInviteCode(code)
	if hasTail {
		masked += "/" + tail
	}
	return masked
}

// isInviteCodeRoute 判断该路由的路径或 body 是否带邀请码：预览、加入空间、提交加入申请。
func isInviteCodeRoute(path string) bool {
	return path == inviteCodeJoinPath || path == inviteCodeJoinRequestPath ||
		strings.HasPrefix(path, inviteCodePreviewPathPrefix)
}

// maskInviteCodeInBody 把 JSON body 里 invite_code 的值打码，保留字段名和其余内容。
func maskInviteCodeInBody(body string) string {
	return inviteCodeFieldRegex.ReplaceAllStringFunc(body, func(m string) string {
		parts := inviteCodeFieldRegex.FindStringSubmatch(m)
		return parts[1] + maskInviteCode(parts[2]) + `"`
	})
}

// readRequestBody 读取请求体（限制大小用于日志，但把剩余字节原样交还后续handler）
func readRequestBody(c *gin.Context) string {
	if c.Request.Body == nil {
		return ""
	}

	// 检查Content-Type，只记录JSON类型
	contentType := c.GetHeader("Content-Type")
	if !strings.Contains(contentType, "application/json") &&
		!strings.Contains(contentType, "application/x-www-form-urlencoded") &&
		!strings.Contains(contentType, "text/") {
		return "[非文本类型，已跳过]"
	}

	// 重置request body：把已窥探的前缀接回未读取的剩余部分，handler 仍能读到完整
	// 数据，但中间件自身只驻留日志需要的那点字节。
	//
	// 这里原本是无上限的 io.ReadAll(c.Request.Body)。Logger 注册在 Auth 之前
	// （router.go NewRouter），因此对所有请求执行，而任何路由的
	// http.MaxBytesReader（limitJSONBody / limitUploadBody / browserskill 的 4 KiB
	// 授权接口）都是在 handler 里才装上的：body 早已被整体读进内存，那些 cap 只剩
	// 报错的意义，没有限住内存的作用。没有 cap 的路由（对话、检索、IM webhook、
	// MCP 端点）则连上限都不存在。
	bodyBytes, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodySize+1))
	originalBody := c.Request.Body
	c.Request.Body = &replayedBody{
		Reader: io.MultiReader(bytes.NewReader(bodyBytes), originalBody),
		Closer: originalBody,
	}
	if err != nil {
		return "[读取请求体失败]"
	}

	// 用于日志的body（限制大小）
	truncated := len(bodyBytes) > maxBodySize
	if truncated {
		bodyBytes = bodyBytes[:maxBodySize]
	}

	bodyStr := string(bodyBytes)
	if truncated {
		bodyStr += bodyTruncatedMarker
	}

	return sanitizeBody(bodyStr)
}

// RequestID middleware adds a unique request ID to the context
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get request ID from header or generate a new one
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = uuid.New().String()
		}
		safeRequestID := secutils.SanitizeForLog(requestID)
		// Set request ID in header
		c.Header("X-Request-ID", requestID)

		// Set request ID in context
		c.Set(types.RequestIDContextKey.String(), requestID)

		// Set logger in context
		requestLogger := logger.GetLogger(c)
		requestLogger = requestLogger.WithField("request_id", safeRequestID)
		c.Set(types.LoggerContextKey.String(), requestLogger)

		// Set request ID in the global context for logging
		c.Request = c.Request.WithContext(
			context.WithValue(
				context.WithValue(c.Request.Context(), types.RequestIDContextKey, requestID),
				types.LoggerContextKey, requestLogger,
			),
		)

		c.Next()
	}
}

// Logger middleware logs request details with request ID, input and output
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		isWikiStats := strings.HasPrefix(path, "/api/v1/knowledgebase/") && strings.HasSuffix(path, "/wiki/stats")
		if strings.HasPrefix(path, "/assets/") || isWikiStats {
			c.Next()
			return
		}

		// Browser traffic contains credentials, page content and screenshots.
		// Keep access metadata, but never read or buffer these request/response bodies.
		browserTraffic := strings.HasPrefix(path, "/api/v1/local-browser/") ||
			path == "/api/v1/me/browser" || strings.HasSuffix(path, "/local-browser")
		// 读取请求体（在Next之前读取，因为Next会消费body）
		var requestBody string
		if !browserTraffic && (c.Request.Method == "POST" || c.Request.Method == "PUT" || c.Request.Method == "PATCH") {
			requestBody = readRequestBody(c)
		}
		// 邀请码是加入共享空间的凭据：这几个路由照常记访问日志，但码只留前缀。
		inviteCodeRoute := isInviteCodeRoute(path)
		if inviteCodeRoute {
			requestBody = maskInviteCodeInBody(requestBody)
			path = maskInviteCodeInPath(path)
		}

		// 创建响应体捕获器
		responseBody := &bytes.Buffer{}
		responseWriter := &loggerResponseBodyWriter{
			ResponseWriter: c.Writer,
			body:           responseBody,
		}
		if !browserTraffic {
			c.Writer = responseWriter
		}

		// Process request
		c.Next()

		// Get request ID from context
		requestID, exists := c.Get(types.RequestIDContextKey.String())
		requestIDStr := "unknown"
		if exists {
			if idStr, ok := requestID.(string); ok && idStr != "" {
				requestIDStr = idStr
			}
		}
		safeRequestID := secutils.SanitizeForLog(requestIDStr)

		// Calculate latency
		latency := time.Since(start)

		// Get client IP and status code
		clientIP := c.ClientIP()
		statusCode := c.Writer.Status()
		method := c.Request.Method

		if raw != "" {
			path = path + "?" + sanitizeQuery(raw)
		}

		// 读取响应体
		responseBodyStr := ""
		if responseBody.Len() > 0 {
			contentType := c.Writer.Header().Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				responseBodyStr = "[SSE流式响应，已跳过]"
			} else if strings.Contains(contentType, "application/json") ||
				strings.Contains(contentType, "text/") {
				bodyBytes := responseBody.Bytes()
				if len(bodyBytes) >= maxBodySize {
					responseBodyStr = string(bodyBytes[:maxBodySize]) + "... [内容过长，已截断]"
				} else {
					responseBodyStr = string(bodyBytes)
				}
				responseBodyStr = sanitizeBody(responseBodyStr)
				if inviteCodeRoute {
					responseBodyStr = maskInviteCodeInBody(responseBodyStr)
				}
			} else {
				responseBodyStr = "[非文本类型，已跳过]"
			}
		}

		// 构建日志消息
		logMsg := logger.GetLogger(c)
		logMsg = logMsg.WithFields(map[string]interface{}{
			"request_id":  safeRequestID,
			"method":      method,
			"path":        secutils.SanitizeForLog(path),
			"status_code": statusCode,
			"size":        c.Writer.Size(),
			"latency":     latency.String(),
			"client_ip":   secutils.SanitizeForLog(clientIP),
		})

		// 添加请求体（如果有）
		if requestBody != "" {
			logMsg = logMsg.WithField("request_body", secutils.SanitizeForLog(requestBody))
		}

		// 添加响应体（如果有）
		if responseBodyStr != "" {
			logMsg = logMsg.WithField("response_body", secutils.SanitizeForLog(responseBodyStr))
		}
		if last := c.Errors.Last(); last != nil && last.Err != nil {
			logMsg = logMsg.WithField("error", secutils.SanitizeForLog(last.Err.Error()))
		}
		switch {
		case statusCode >= 500:
			logMsg.Error()
		case statusCode >= 400:
			logMsg.Warn()
		default:
			logMsg.Info()
		}
	}
}
