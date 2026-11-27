package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/primind/primind/internal/application/access"
	"github.com/primind/primind/internal/application/service"
	"github.com/primind/primind/internal/errors"
	"github.com/primind/primind/internal/event"
	"github.com/primind/primind/internal/infrastructure/docparser"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/models/api"
	"github.com/primind/primind/internal/sandbox"
	"github.com/primind/primind/internal/storageurl"
	"github.com/primind/primind/internal/stream"
	"github.com/primind/primind/internal/tracing/langfuse"
	"github.com/primind/primind/internal/turn"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	secutils "github.com/primind/primind/internal/utils"
)

const (
	maxAttachmentUploadsPerRequest = 5
	maxAttachmentUploadTotalBytes  = int64(100 * 1024 * 1024)
)

// qaRequestContext holds all the common data needed for QA requests
type qaRequestContext struct {
	ctx                   context.Context
	c                     *gin.Context
	sessionID             string
	requestID             string
	receivedAt            time.Time // Wall-clock time the handler started processing the request
	query                 string    // Question sent to models; the upload-only question when the user typed nothing
	userInput             string    // Text the user typed, stored on the user message; may be empty
	session               *types.Session
	customAgent           *types.CustomAgent
	assistantMessage      *types.Message
	knowledgeBaseIDs      []string
	knowledgeIDs          []string
	tagScopes             []types.TagScope
	tagIDs                []string
	mcpServiceIDs         []string
	skillNames            []string
	summaryModelID        string
	reasoningEffort       string
	localBrowserEnabled   bool
	webSearchEnabled      bool
	mentionedItems        types.MentionedItems
	effectiveTenantID     uint64                   // when using shared agent, tenant ID for model/KB/MCP resolution; 0 = use context tenant
	sharedAgentReadOnly   bool                     // access was granted by a read-only agent share
	images                []ImageAttachment        // Uploaded images with analysis text
	userMessageID         string                   // Created user message ID (populated after createUserMessage)
	userCreatedAt         time.Time                // Persisted user message timestamp, echoed on agent_query
	channel               string                   // Source channel: "web", "api", "im", etc.
	attachments           types.MessageAttachments // Processed base64 file attachments (legacy inline uploads)
	attachmentIDs         []string                 // Pre-uploaded session-scoped document IDs, resolved after SSE starts
	attachmentMetas       types.MessageAttachments // Metadata-only view of attachmentIDs for the persisted user message
	suggestionAttribution *types.SuggestionAttribution
	questionOrigin        *types.QuestionOrigin
	// resourceRewriter turns internal storage references in the outbound stream
	// into directly loadable URLs when the caller asks for `resource_urls=public`.
	// Disabled (a pass-through) in the default handle mode.
	resourceRewriter *storageurl.StreamRewriter

	// steerSink bridges the engine's round-boundary drain to the steer
	// sub-list and user-row persistence. Set by setupSSEStream for agent
	// runs only.
	steerSink *steerSink

	// Snapshot of the request fields needed to persist the input-bar state
	// for session restoration. Kept verbatim from the request so we record
	// what the user had selected on the UI (not server-side resolutions).
	reqAgentEnabled bool
	reqAgentID      string

	// skipSSE is set for server-started follow-up runs (steer backlog after
	// the previous turn exits). Events still land in StreamManager so the
	// client can attach via continue-stream; nothing is written to gin.
	skipSSE bool
	// steerCarryOver is appended to this run's steer sub-list before the run
	// is published as live, so leftover inject / after messages from the
	// previous run are visible to this engine from round 1 and to any client
	// that reloads the queue.
	steerCarryOver []interfaces.StreamEvent
	// turnLeaseHeld records that executeQA already took the send-side turn
	// lease for this request, so the QA service does not take a second one.
	turnLeaseHeld bool
}

// buildQARequest converts the qaRequestContext into a types.QARequest for service invocation.
// steerSink rides along when the caller set one, so agent runs drain mid-run
// message injections at round boundaries.
func (rc *qaRequestContext) buildQARequest() *types.QARequest {
	imageURLs, imageDescription := extractImageURLsAndOCRText(rc.images)
	req := &types.QARequest{
		Session:             rc.session,
		Query:               rc.query,
		AssistantMessageID:  rc.assistantMessage.ID,
		SummaryModelID:      rc.summaryModelID,
		ReasoningEffort:     rc.reasoningEffort,
		CustomAgent:         rc.customAgent,
		SharedAgentReadOnly: rc.sharedAgentReadOnly,
		KnowledgeBaseIDs:    rc.knowledgeBaseIDs,
		KnowledgeIDs:        rc.knowledgeIDs,
		TagScopes:           rc.tagScopes,
		MCPServiceIDs:       rc.mcpServiceIDs,
		SkillNames:          rc.skillNames,
		ImageURLs:           imageURLs,
		ImageDescription:    imageDescription,
		UserMessageID:       rc.userMessageID,
		WebSearchEnabled:    rc.webSearchEnabled,
		LocalBrowserEnabled: rc.localBrowserEnabled,
		Attachments:         rc.attachments,
		QuestionOrigin:      rc.questionOrigin,
		TurnLeaseHeld:       rc.turnLeaseHeld,
	}
	if rc.steerSink != nil {
		req.SteerSink = rc.steerSink
	}
	return req
}

// uploadOnlyQuery returns the question used for a request that carries inline
// image or file content but no text, or "" when it carries neither. URL-only
// images do not count: parseQARequest strips client-supplied image URLs.
// Pre-uploaded attachment IDs do not count either: those documents can still
// fail or time out after the stream starts, leaving nothing to answer from.
func uploadOnlyQuery(ctx context.Context, req *CreateKnowledgeQARequest) string {
	hasUpload := false
	for _, img := range req.Images {
		hasUpload = hasUpload || strings.TrimSpace(img.Data) != ""
	}
	for _, att := range req.AttachmentUploads {
		hasUpload = hasUpload || strings.TrimSpace(att.Data) != ""
	}
	if !hasUpload {
		return ""
	}
	return types.UploadOnlyQuestion(types.LanguageFromContextOrDefault(ctx))
}

// parseQARequest parses and validates a QA request, returns the request context
func (h *Handler) parseQARequest(c *gin.Context, logPrefix string) (*qaRequestContext, *CreateKnowledgeQARequest, error) {
	receivedAt := time.Now()
	ctx := logger.CloneContext(c.Request.Context())
	requestID := secutils.SanitizeForLog(c.GetString(types.RequestIDContextKey.String()))
	logger.Infof(ctx, "[%s] TTFB:start request_id=%s received_at=%d",
		logPrefix, requestID, receivedAt.UnixMilli())

	// Get session ID from URL parameter
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		return nil, nil, errors.NewBadRequestError(errors.ErrInvalidSessionID.Error())
	}

	// Parse request body
	var request CreateKnowledgeQARequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		return nil, nil, errors.NewBadRequestError(err.Error())
	}

	level, validEffort := api.ParseReasoningEffort(request.ReasoningEffort)
	if !validEffort {
		return nil, nil, errors.NewBadRequestError(
			fmt.Sprintf("reasoning_effort must be one of %v", api.AllReasoningEfforts),
		)
	}
	request.ReasoningEffort = string(level)

	// Validate syntax before session lookup or QA work, preserving the original
	// query text. KnowledgeQA applies its XSS-pattern check later in the chat
	// pipeline; AgentQA must allow frontend code in conversation text.
	validatedQuery, valid := secutils.ValidateInputSyntax(request.Query)
	if !valid {
		logger.Error(ctx, "Query content is invalid")
		return nil, nil, errors.NewBadRequestError("Query content contains invalid content")
	}
	modelQuery := request.Query
	if validatedQuery == "" {
		modelQuery = uploadOnlyQuery(ctx, &request)
		if modelQuery == "" {
			logger.Error(ctx, "Query content is empty")
			return nil, nil, errors.NewBadRequestError("Query content cannot be empty")
		}
		request.Query = ""
	}

	// Resolve the storage-reference representation up front: once the SSE stream
	// has started an invalid value can no longer be reported as a 400.
	resourceRewriter, err := h.resolveStreamRewriter(c)
	if err != nil {
		logger.Warnf(ctx, "Rejected resource URL mode: %v", err)
		return nil, nil, err
	}
	if h.suggestionService != nil && request.SuggestionAttribution != nil {
		if err := h.suggestionService.ValidateAttribution(ctx, sessionID, request.Query, request.SuggestionAttribution); err != nil {
			return nil, nil, errors.NewBadRequestError("invalid suggestion attribution")
		}
	}

	// SSRF protection: strip client-supplied URL/Caption fields from image attachments.
	// The URL field must only be populated server-side by saveImageAttachments; an
	// attacker could inject internal network URLs to trigger SSRF via the LLM provider.
	for i := range request.Images {
		request.Images[i].URL = ""
		request.Images[i].Caption = ""
	}

	// Log request details
	if requestJSON, err := json.Marshal(request); err == nil {
		logger.Infof(ctx, "[%s] Request: session_id=%s, request=%s",
			logPrefix, sessionID, secutils.SanitizeForLog(secutils.CompactImageDataURLForLog(string(requestJSON))))
	}

	// Get session. QA writes new messages into the session, so use the strict
	// owner scope: a tenant admin may read an API-key session but must not be
	// able to post messages to it (which would otherwise fail later at message
	// creation with a 500 instead of a clean not-found).
	session, err := h.sessionService.GetOwnedSession(ctx, sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session, session ID: %s, error: %v", sessionID, err)
		return nil, nil, errors.NewNotFoundError("Session not found")
	}

	// Get custom agent if agent_id is provided. Backend resolves shared agent from share relation (no client-provided tenant).
	customAgent, effectiveTenantID, sharedAgentReadOnly := h.resolveAgent(ctx, c, request.AgentID, request.AgentSourceTenantID)
	if request.AgentSourceTenantID != 0 && customAgent == nil {
		return nil, nil, errors.NewNotFoundError("Shared agent not found")
	}

	// Merge @mentioned items into knowledge_base_ids and knowledge_ids
	kbIDs, knowledgeIDs := mergeKnowledgeTargets(request.KnowledgeBaseIDs, request.KnowledgeIDs, request.MentionedItems)
	if err := types.AuthorizeTenantAPIKeyKnowledgeTargets(ctx, kbIDs, knowledgeIDs); err != nil {
		return nil, nil, err
	}

	// The built-in wiki fixer is invoked from a KB page, not from a tenant's
	// regular agent picker. When the KB is shared, run it in the source tenant
	// only if the caller has edit permission, so KB-scoped models/tools resolve
	// without granting viewers write capability.
	if customAgent != nil && customAgent.ID == types.BuiltinWikiFixerID {
		if scopedAgent, scopedTenantID := h.resolveWikiFixerTenantScope(
			ctx,
			customAgent,
			c.GetUint64(types.TenantIDContextKey.String()),
			types.TenantRoleFromContext(ctx),
			kbIDs,
		); scopedTenantID != 0 {
			customAgent = scopedAgent
			effectiveTenantID = scopedTenantID
			sharedAgentReadOnly = false
		}
	}

	// A per-request model override resolves in the workspace the run executes
	// in. For a shared agent, or the wiki fixer moved into a shared KB's
	// workspace, that is the owner's, where it could select any of the owner's
	// models; it would also be stored as the message's model for follow-ups.
	if effectiveTenantID != 0 && effectiveTenantID != c.GetUint64(types.TenantIDContextKey.String()) {
		request.SummaryModelID = ""
	}

	if request.LocalBrowserEnabled && (customAgent == nil || !customAgent.IsAgentMode()) {
		return nil, nil, errors.NewBadRequestError("Local browser requires an agent with tool calling enabled")
	}

	// Log merge results for debugging
	logger.Infof(ctx, "[%s] @mention merge: request.KnowledgeBaseIDs=%v, request.MentionedItems=%d, merged kbIDs=%v, merged knowledgeIDs=%v",
		logPrefix, request.KnowledgeBaseIDs, len(request.MentionedItems), kbIDs, knowledgeIDs)

	// Process inline base64 images: decode and save to storage.
	// VLM analysis for RAG paths is deferred to the pipeline rewrite step.
	// For pure chat paths with non-vision models, VLM analysis runs here as fallback.
	if len(request.Images) > 0 {
		if customAgent == nil || !customAgent.Config.ImageUploadEnabled {
			logger.Warnf(ctx, "[%s] Image upload is not enabled for this agent, rejecting %d images", logPrefix, len(request.Images))
			return nil, nil, errors.NewBadRequestError("Image upload is not enabled for this agent")
		}
		tenantID := c.GetUint64(types.TenantIDContextKey.String())
		agentStorageProvider := customAgent.Config.ImageStorageProvider
		if err := h.saveImageAttachments(ctx, request.Images, tenantID, agentStorageProvider); err != nil {
			logger.Errorf(ctx, "[%s] Failed to save images: %v", logPrefix, err)
			return nil, nil, errors.NewBadRequestError(fmt.Sprintf("Image save failed: %v", err))
		}

		// VLM analysis is always deferred to after SSE stream is up:
		// - Agent mode: runs in async execution flow with tool_call/tool_result events
		// - Normal RAG mode: runs in the pipeline rewrite step with progress events
		// - Normal pure-chat mode: runs in the async goroutine with progress events
	}

	// Process file attachments: decode and save to storage, extract content
	var processedAttachments types.MessageAttachments
	if len(request.AttachmentUploads) > 0 {
		logger.Infof(ctx, "[%s] processing %d attachment(s)", logPrefix, len(request.AttachmentUploads))

		// Decode first and validate actual bytes. Client-declared file_size is
		// metadata only and must not be trusted for memory or sandbox limits.
		maxSize := secutils.GetMaxFileSize()
		decodedAttachments, decodeErr := decodeAndValidateAttachmentUploads(
			request.AttachmentUploads,
			maxAttachmentUploadsPerRequest,
			maxSize,
			maxAttachmentUploadTotalBytes,
		)
		if decodeErr != nil {
			return nil, nil, errors.NewBadRequestError(decodeErr.Error())
		}

		tenantID := c.GetUint64(types.TenantIDContextKey.String())
		attachmentRuntimeCtx := ctx
		if effectiveTenantID != 0 {
			attachmentRuntimeCtx = types.WithExecutionTenant(ctx, effectiveTenantID)
		}

		// Use ASR only when the agent has audio upload enabled.
		asrModelID := ""
		if customAgent != nil && customAgent.Config.AudioUploadEnabled && customAgent.Config.ASRModelID != "" {
			asrModelID = customAgent.Config.ASRModelID
		}

		// Resolve the agent's chat parser engine from attachment file types
		if customAgent != nil {
			for _, att := range request.AttachmentUploads {
				ext := strings.ToLower(filepath.Ext(att.FileName))
				if engine := customAgent.Config.ResolveChatParserEngine(ext); engine != "" {
					attachmentRuntimeCtx = context.WithValue(attachmentRuntimeCtx, types.ChatParserEngineContextKey, engine)
					break
				}
			}
		}

		// Process all attachments concurrently.
		processedAttachments = make(types.MessageAttachments, len(request.AttachmentUploads))
		var wg sync.WaitGroup
		errChan := make(chan error, len(request.AttachmentUploads))

		for i, upload := range request.AttachmentUploads {
			wg.Add(1)
			go func(idx int, att AttachmentUpload) {
				defer wg.Done()

				processed, err := h.attachmentProcessor.ProcessAttachment(
					attachmentRuntimeCtx, decodedAttachments[idx], att.FileName, int64(len(decodedAttachments[idx])), tenantID, asrModelID,
				)
				if err != nil {
					errChan <- fmt.Errorf("attachment %d processing failed: %w", idx+1, err)
					return
				}

				processedAttachments[idx] = *processed
			}(i, upload)
		}

		wg.Wait()
		close(errChan)

		if len(errChan) > 0 {
			err := <-errChan
			logger.Errorf(ctx, "[%s] attachment processing failed: %v", logPrefix, err)
			return nil, nil, errors.NewBadRequestError(fmt.Sprintf("attachment processing failed: %v", err))
		}

		logger.Infof(ctx, "[%s] all attachments processed", logPrefix)
	}

	// Pre-uploaded documents may still be parsing. Only fetch their metadata
	// here (fast, available even while processing) to validate agent file-type
	// limits and to record the attachments on the persisted user message. The
	// heavy content selection happens after the SSE stream is up, so the send
	// is not blocked while parsing finishes (see resolveTemporaryAttachments).
	var attachmentIDs []string
	var attachmentMetas types.MessageAttachments
	if len(request.AttachmentIDs) > 0 {
		normalizedIDs, normErr := normalizeTemporaryAttachmentIDs(request.AttachmentIDs)
		if normErr != nil {
			return nil, nil, errors.NewBadRequestError(normErr.Error())
		}
		tenantID := session.TenantID
		attachmentMetas = make(types.MessageAttachments, 0, len(normalizedIDs))
		for _, id := range normalizedIDs {
			doc, getErr := h.temporaryDocuments.Get(ctx, tenantID, sessionID, id)
			if getErr != nil || doc == nil {
				return nil, nil, errors.NewBadRequestError(
					fmt.Sprintf("attachment %s was not found in this session", secutils.SanitizeForLog(id)))
			}
			if customAgent != nil && len(customAgent.Config.SupportedFileTypes) > 0 {
				ext := strings.TrimPrefix(strings.ToLower(doc.FileType), ".")
				if !containsFileType(customAgent.Config.SupportedFileTypes, ext) {
					return nil, nil, errors.NewBadRequestError(
						fmt.Sprintf("file type %s is not supported by this agent", ext))
				}
			}
			attachmentMetas = append(attachmentMetas, types.MessageAttachment{
				ID: doc.ID, URL: doc.ResourceRef, FileName: doc.FileName,
				FileType: doc.FileType, FileSize: doc.FileSize,
			})
		}
		attachmentIDs = normalizedIDs
	}

	mentionScopes := tagScopesFromMentionedItems(request.MentionedItems)
	requestTagIDs := dedupRequestStrings(request.TagIDs)
	if err := validateUnscopedTagIDs(orphanTagIDsForScope(requestTagIDs, mentionScopes), secutils.SanitizeForLogArray(kbIDs)); err != nil {
		return nil, nil, errors.NewBadRequestError(err.Error())
	}
	tagScopes := mergeTagScopesFromRequestIDs(mentionScopes, requestTagIDs, secutils.SanitizeForLogArray(kbIDs))
	tagIDs := dedupRequestStrings(append(request.TagIDs, mentionedIDsByType(request.MentionedItems, "tag")...))
	mcpServiceIDs := dedupRequestStrings(append(request.MCPServiceIDs, mentionedIDsByType(request.MentionedItems, "mcp")...))
	skillNames := dedupRequestStrings(append(request.SkillNames, mentionedIDsByType(request.MentionedItems, "skill")...))
	executionContext, agentID, agentTenantID, modelID := buildMessageExecutionContext(
		ctx,
		customAgent,
		effectiveTenantID,
		request.SummaryModelID,
		secutils.SanitizeForLogArray(kbIDs),
		secutils.SanitizeForLogArray(knowledgeIDs),
		secutils.SanitizeForLogArray(tagIDs),
		tagScopes,
		secutils.SanitizeForLogArray(mcpServiceIDs),
		secutils.SanitizeForLogArray(skillNames),
		request.WebSearchEnabled,
	)

	executionContext.LocalBrowserEnabled = request.LocalBrowserEnabled

	// Build request context
	reqCtx := &qaRequestContext{
		ctx:         ctx,
		c:           c,
		sessionID:   sessionID,
		requestID:   requestID,
		receivedAt:  receivedAt,
		query:       modelQuery,
		userInput:   request.Query,
		session:     session,
		customAgent: customAgent,
		assistantMessage: &types.Message{
			SessionID:        sessionID,
			Role:             "assistant",
			RequestID:        c.GetString(types.RequestIDContextKey.String()),
			IsCompleted:      false,
			Channel:          request.Channel,
			AgentID:          agentID,
			AgentTenantID:    agentTenantID,
			ModelID:          modelID,
			ExecutionContext: executionContext,
		},
		knowledgeBaseIDs:      secutils.SanitizeForLogArray(kbIDs),
		knowledgeIDs:          secutils.SanitizeForLogArray(knowledgeIDs),
		tagScopes:             tagScopes,
		tagIDs:                secutils.SanitizeForLogArray(tagIDs),
		mcpServiceIDs:         secutils.SanitizeForLogArray(mcpServiceIDs),
		skillNames:            secutils.SanitizeForLogArray(skillNames),
		summaryModelID:        secutils.SanitizeForLog(request.SummaryModelID),
		reasoningEffort:       request.ReasoningEffort,
		webSearchEnabled:      request.WebSearchEnabled,
		localBrowserEnabled:   request.LocalBrowserEnabled,
		mentionedItems:        convertMentionedItems(request.MentionedItems),
		effectiveTenantID:     effectiveTenantID,
		sharedAgentReadOnly:   sharedAgentReadOnly,
		images:                request.Images,
		channel:               request.Channel,
		attachments:           processedAttachments,
		attachmentIDs:         attachmentIDs,
		attachmentMetas:       attachmentMetas,
		suggestionAttribution: request.SuggestionAttribution,
		questionOrigin:        request.QuestionOrigin,
		reqAgentEnabled:       request.AgentEnabled,
		reqAgentID:            request.AgentID,
		resourceRewriter:      resourceRewriter,
	}

	return reqCtx, &request, nil
}

func decodeAndValidateAttachmentUploads(
	uploads []AttachmentUpload,
	maxCount int,
	maxFileBytes, maxTotalBytes int64,
) ([][]byte, error) {
	if len(uploads) > maxCount {
		return nil, fmt.Errorf("at most %d attachments are allowed per request", maxCount)
	}
	decoded := make([][]byte, len(uploads))
	var total int64
	for i, upload := range uploads {
		data, err := DecodeBase64Attachment(upload.Data)
		if err != nil {
			return nil, fmt.Errorf("attachment %d decode failed: %w", i+1, err)
		}
		actualSize := int64(len(data))
		if actualSize > maxFileBytes {
			return nil, fmt.Errorf("attachment %d exceeds size limit of %d bytes", i+1, maxFileBytes)
		}
		total += actualSize
		if total > maxTotalBytes {
			return nil, fmt.Errorf("attachments exceed total request limit of %d bytes", maxTotalBytes)
		}
		decoded[i] = data
	}
	return decoded, nil
}

func buildMessageExecutionContext(
	ctx context.Context,
	agent *types.CustomAgent,
	effectiveTenantID uint64,
	modelOverride string,
	knowledgeBaseIDs []string,
	knowledgeIDs []string,
	tagIDs []string,
	tagScopes []types.TagScope,
	mcpServiceIDs []string,
	skillNames []string,
	webSearchEnabled bool,
) (types.MessageExecutionContext, string, uint64, string) {
	locale := types.LanguageFromContextOrDefault(ctx)

	snapshot := types.MessageExecutionContext{
		KnowledgeBaseIDs:    knowledgeBaseIDs,
		KnowledgeIDs:        knowledgeIDs,
		TagIDs:              tagIDs,
		TagScopes:           cloneTagScopes(tagScopes),
		MCPServiceIDs:       mcpServiceIDs,
		SkillNames:          skillNames,
		WebSearchEnabled:    webSearchEnabled,
		Locale:              locale,
		LangfuseTraceparent: langfuse.TraceparentFromContext(ctx),
	}
	if agent == nil {
		return snapshot, "", effectiveTenantID, modelOverride
	}

	modelID := modelOverride
	if modelID == "" {
		modelID = agent.Config.ModelID
	}
	agentTenantID := effectiveTenantID
	if agentTenantID == 0 {
		agentTenantID = agent.TenantID
	}

	// Marshal/unmarshal gives the snapshot independent backing slices, so a
	// later agent edit cannot mutate an in-flight message context.
	if agent.Config.QuestionSuggestions != nil {
		if encoded, err := json.Marshal(agent.Config.QuestionSuggestions); err == nil {
			var suggestions types.QuestionSuggestionConfig
			if json.Unmarshal(encoded, &suggestions) == nil {
				snapshot.QuestionSuggestions = &suggestions
			}
		}
	}
	hashInput := struct {
		QuestionSuggestions *types.QuestionSuggestionConfig `json:"question_suggestions,omitempty"`
		KnowledgeBaseIDs    []string                        `json:"knowledge_base_ids,omitempty"`
		KnowledgeIDs        []string                        `json:"knowledge_ids,omitempty"`
		TagIDs              []string                        `json:"tag_ids,omitempty"`
		TagScopes           []types.TagScope                `json:"tag_scopes,omitempty"`
		ModelID             string                          `json:"model_id,omitempty"`
	}{
		QuestionSuggestions: snapshot.QuestionSuggestions,
		KnowledgeBaseIDs:    knowledgeBaseIDs,
		KnowledgeIDs:        knowledgeIDs,
		TagIDs:              tagIDs,
		TagScopes:           snapshot.TagScopes,
		ModelID:             modelID,
	}
	if encoded, err := json.Marshal(hashInput); err == nil {
		hash := sha256.Sum256(encoded)
		snapshot.AgentConfigHash = fmt.Sprintf("%x", hash[:])
	}

	return snapshot, agent.ID, agentTenantID, modelID
}

func cloneTagScopes(scopes []types.TagScope) []types.TagScope {
	if len(scopes) == 0 {
		return nil
	}
	cloned := make([]types.TagScope, 0, len(scopes))
	for _, scope := range scopes {
		if scope.KnowledgeBaseID == "" || len(scope.TagIDs) == 0 {
			continue
		}
		cloned = append(cloned, types.TagScope{
			KnowledgeBaseID: scope.KnowledgeBaseID,
			TagIDs:          append([]string(nil), scope.TagIDs...),
		})
	}
	return cloned
}

// resolveAgent resolves the custom agent by ID through service.AgentAccessResolver,
// the one component every agent-by-ID route shares: the caller's own agent unless
// a source workspace is given, otherwise (or when no own agent matches) a share.
// Returns (nil, 0) if agentID is empty or not found.
func (h *Handler) resolveAgent(
	ctx context.Context,
	c *gin.Context,
	agentID string,
	sourceTenantID uint64,
) (*types.CustomAgent, uint64, bool) {
	if agentID == "" {
		return nil, 0, false
	}

	if h.agentAccessResolver == nil {
		logger.Errorf(ctx, "Agent access resolver is not configured, agent ID: %s, using default config",
			secutils.SanitizeForLog(agentID))
		return nil, 0, false
	}

	// ResolveForChat: chat does not refuse a share this workspace has disabled.
	// That predates the disabled list and stays as it is until the recorded
	// defect (docs/bug/2026-11-26-本空间停用的共享智能体仍可在对话中使用.md) is fixed.
	resolved, err := h.agentAccessResolver.ResolveForChat(ctx, agentCallerFromGin(c), agentID, sourceTenantID)
	if err != nil {
		logger.Warnf(ctx, "Failed to get agent, agent ID: %s, source tenant: %d, error: %v, using default config",
			secutils.SanitizeForLog(agentID), sourceTenantID, err)
		return nil, 0, false
	}
	if resolved.IsShared {
		return resolved.Agent, resolved.EffectiveTenantID, true
	}
	return resolved.Agent, 0, false
}

// agentCallerFromGin snapshots the identity the resolver keys on: the workspace
// and user auth attached to the request, and the user's role in that workspace.
func agentCallerFromGin(c *gin.Context) service.AgentCaller {
	userID, _ := c.Get(types.UserIDContextKey.String())
	id, _ := userID.(string)
	return service.AgentCaller{
		TenantID: c.GetUint64(types.TenantIDContextKey.String()),
		UserID:   id,
		Role:     types.TenantRoleFromContext(c.Request.Context()),
	}
}

// mergeKnowledgeTargets merges request KB/knowledge IDs with @mentioned items into deduplicated slices.
func mergeKnowledgeTargets(requestKBIDs []string, requestKnowledgeIDs []string, mentionedItems []MentionedItemRequest) (kbIDs []string, knowledgeIDs []string) {
	kbIDSet := make(map[string]bool)
	kbIDs = make([]string, 0, len(requestKBIDs)+len(mentionedItems))
	for _, id := range requestKBIDs {
		if id != "" && !kbIDSet[id] {
			kbIDs = append(kbIDs, id)
			kbIDSet[id] = true
		}
	}

	knowledgeIDSet := make(map[string]bool)
	knowledgeIDs = make([]string, 0, len(requestKnowledgeIDs)+len(mentionedItems))
	for _, id := range requestKnowledgeIDs {
		if id != "" && !knowledgeIDSet[id] {
			knowledgeIDs = append(knowledgeIDs, id)
			knowledgeIDSet[id] = true
		}
	}

	for _, item := range mentionedItems {
		if item.ID == "" {
			continue
		}
		switch item.Type {
		case "kb":
			if !kbIDSet[item.ID] {
				kbIDs = append(kbIDs, item.ID)
				kbIDSet[item.ID] = true
			}
		case "file":
			if !knowledgeIDSet[item.ID] {
				knowledgeIDs = append(knowledgeIDs, item.ID)
				knowledgeIDSet[item.ID] = true
			}
		}
	}
	return kbIDs, knowledgeIDs
}

// sseStreamContext holds the context for SSE streaming
type sseStreamContext struct {
	eventBus         *event.EventBus
	streamHandler    *AgentStreamHandler
	asyncCtx         context.Context
	cancel           context.CancelFunc
	assistantMessage *types.Message
	// steerSink bridges the engine's round-boundary drain to the steer
	// sub-list and user-row persistence. Nil for non-agent modes.
	steerSink *steerSink
	// liveRunFailed is set when SetLiveRun failed. executeQA must not start
	// the engine: /steer would see no marker and the client would start a
	// second turn.
	liveRunFailed bool
	liveRunErr    error
	// releaseTurn ends the sandbox turn lease taken before persist. Knowledge
	// mode releases on completion (the pipeline returns before the stream
	// finishes); agent mode releases when AgentQA returns.
	releaseTurn func()
	// unregisterTurn removes the turn from the in-process registry once it
	// reached a terminal state (see finalizeTurn / Handler.turns).
	unregisterTurn func()
	// unregisterOnce guards unregisterTurn: several paths can end a turn
	// (success, error, stop, backstop) and all of them call endTurn.
	unregisterOnce sync.Once
	// stopLiveRunHeartbeat ends the goroutine that keeps this run's live-run
	// marker alive (see startLiveRunHeartbeat). Nil when no marker was set.
	stopLiveRunHeartbeat chan struct{}
	// turnMu serialises the turn-state decisions on assistantMessage (status,
	// error code, is_completed) and the writes that store them: the engine
	// goroutine, a stop and a shutdown interrupt can end the same turn at once.
	turnMu sync.Mutex
	// shutdownInterrupted is set when graceful shutdown cancelled the turn:
	// the cancellation then ends it as interrupted, not stopped.
	shutdownInterrupted atomic.Bool
}

// endTurn marks the turn as no longer generating in this process. Safe to
// call from any path and more than once.
func (sc *sseStreamContext) endTurn() {
	if sc == nil {
		return
	}
	sc.unregisterOnce.Do(func() {
		if sc.unregisterTurn != nil {
			sc.unregisterTurn()
		}
		if sc.stopLiveRunHeartbeat != nil {
			close(sc.stopLiveRunHeartbeat)
		}
	})
}

// liveRunHeartbeatInterval is how often a generating agent run re-extends
// its live-run marker. It must stay well below the stream TTL (1h) so a
// run that is silent for a long time (a slow tool call, a long thinking
// phase) keeps its marker. A variable so tests can shorten it.
var liveRunHeartbeatInterval = 5 * time.Minute

// startLiveRunHeartbeat keeps the run's live-run marker alive for as long as
// this process is generating it (BF-03).
//
// The marker used to be extended by every read of the session's stream, so a
// killed engine's marker never expired while anyone watched the session, and
// continue-stream self-heal and the turn reconciler both took the dead run for
// a live one. Reads are now pure; only the generating process extends its own
// marker, here, and stops when the turn ends. If the process dies, the
// heartbeat dies with it and the marker expires one TTL after the last beat.
// turn.MaxTurnAge bounds a run whose teardown never ran.
func (h *Handler) startLiveRunHeartbeat(ctx context.Context, streamCtx *sseStreamContext, sessionID, messageID string) {
	if h.streamManager == nil || streamCtx == nil || streamCtx.stopLiveRunHeartbeat != nil {
		return
	}
	stop := make(chan struct{})
	streamCtx.stopLiveRunHeartbeat = stop
	ctx = context.WithoutCancel(ctx)
	go func() {
		ticker := time.NewTicker(liveRunHeartbeatInterval)
		defer ticker.Stop()
		deadline := time.NewTimer(turn.MaxTurnAge)
		defer deadline.Stop()
		for {
			select {
			case <-stop:
				return
			case <-deadline.C:
				logger.Warnf(ctx, "live run heartbeat for session %s stopped after %s without the turn ending",
					sessionID, turn.MaxTurnAge)
				return
			case <-ticker.C:
				if err := h.streamManager.TouchLiveRun(ctx, sessionID, messageID); err != nil {
					logger.Warnf(ctx, "live run heartbeat failed for session %s: %v", sessionID, err)
				}
			}
		}
	}()
}

// setupSSEStream sets up the SSE streaming context
func (h *Handler) setupSSEStream(reqCtx *qaRequestContext, generateTitle bool, mode qaMode) *sseStreamContext {
	// Base context for async work: when using shared agent, use source tenant for model/KB/MCP resolution
	baseCtx := reqCtx.ctx
	if reqCtx.effectiveTenantID != 0 && h.tenantService != nil {
		if tenant, err := h.tenantService.GetTenantByID(reqCtx.ctx, reqCtx.effectiveTenantID); err == nil && tenant != nil {
			baseCtx = types.WithExecutionTenant(reqCtx.ctx, reqCtx.effectiveTenantID)
			if reqCtx.customAgent != nil {
				baseCtx = access.WithSharedAgent(baseCtx, reqCtx.customAgent)
			}
			baseCtx = context.WithValue(baseCtx, types.TenantInfoContextKey, tenant)
			logger.Infof(
				reqCtx.ctx,
				"Using effective tenant %d for shared agent (model/KB/MCP)",
				reqCtx.effectiveTenantID,
			)
		}
	}
	// The session's sandbox stays bound to the session owner even when the
	// borrowed tenant above drives everything else, because DeleteSession tears
	// that sandbox down from a request that only knows the session's tenant.
	baseCtx = types.WithSandboxTenantID(baseCtx, reqCtx.session.TenantID)

	// An agent that opted out of long-term memory has to be opted out of the
	// write path too, not just recall. The two run from different contexts:
	// recall is marked inside the QA services, while extraction, the explicit
	// "remember this" route and document affinity are all kicked off from
	// completeAssistantMessage on a context descended from this one. Marking
	// the root of the async work is what keeps them from disagreeing — an
	// agent that cannot read the memory must not keep writing to it.
	if reqCtx.customAgent != nil {
		baseCtx = types.ApplyAgentMemoryPreference(baseCtx, reqCtx.customAgent.Config.MemoryEnabled)
	}

	eventBus := event.NewEventBus()
	asyncCtx, cancel := context.WithCancel(logger.CloneContext(baseCtx))

	streamCtx := &sseStreamContext{
		eventBus:         eventBus,
		asyncCtx:         asyncCtx,
		cancel:           cancel,
		assistantMessage: reqCtx.assistantMessage,
	}

	// Mid-run steering: only ReAct agent turns (qaModeAgent) have an engine
	// loop to drain at and a teardown that ClearLiveRun / kick / discard.
	// A custom agent configured as quick-answer still has customAgent != nil
	// but executeQA runs KnowledgeQA — publishing a sink there parks /steer
	// messages until the Redis TTL expires. The sink is also mirrored onto
	// reqCtx so the async goroutine's buildQARequest() picks it up — the
	// streamCtx copy alone is never read by the QA call path.
	//
	// SetLiveRun happens before SSE headers: a 409/503 after text/event-stream
	// has started cannot change the status, and the client would sit on a
	// stream nobody will write to.
	if mode == qaModeAgent && reqCtx.customAgent != nil {
		streamCtx.steerSink = newSteerSink(
			asyncCtx, reqCtx.sessionID, reqCtx.requestID, reqCtx.assistantMessage,
			h.messageService, h.streamManager,
		)
		reqCtx.steerSink = streamCtx.steerSink

		if err := h.streamManager.SetLiveRun(
			logger.CloneContext(baseCtx), reqCtx.sessionID,
			reqCtx.assistantMessage.ID, reqCtx.requestID,
		); err != nil {
			logger.ErrorWithFields(reqCtx.ctx, err, map[string]interface{}{
				"session_id": reqCtx.sessionID,
			})
			streamCtx.liveRunFailed = true
			streamCtx.liveRunErr = err
			return streamCtx
		}
		// Only this process may keep the marker alive; reads no longer do.
		h.startLiveRunHeartbeat(baseCtx, streamCtx, reqCtx.sessionID, reqCtx.assistantMessage.ID)
	}

	if !reqCtx.skipSSE {
		setSSEHeaders(reqCtx.c)
	}

	h.writeAgentQueryEvent(
		reqCtx.ctx,
		reqCtx.sessionID,
		reqCtx.userMessageID,
		reqCtx.userCreatedAt,
		reqCtx.assistantMessage,
	)

	// Setup stop event handler
	h.setupStopEventHandler(eventBus, reqCtx.session.TenantID, streamCtx)

	// Watch for stop events independently of the client SSE connection so a
	// user-requested stop reliably cancels generation even when the client
	// has already disconnected (e.g. API-Key callers that close the stream
	// before POSTing /stop). The watcher self-terminates on a terminal stream
	// event, so its lifetime is decoupled from when the QA service call
	// returns (KnowledgeQA returns immediately while streaming continues in a
	// background goroutine, whereas AgentQA blocks until done). Use a
	// connection-independent context derived from baseCtx so it survives the
	// client disconnect.
	h.startStopWatcher(logger.CloneContext(baseCtx), reqCtx.sessionID, reqCtx.assistantMessage.ID, eventBus)

	// Registered before the stream handler: the bus runs listeners in order,
	// so the failed state is stored before the error frame reaches the
	// client and a reload right after it never shows a generating turn.
	h.watchTurnErrors(streamCtx)

	// Setup stream handler
	streamCtx.streamHandler = h.setupStreamHandler(asyncCtx, reqCtx.sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, reqCtx.session.TenantID, reqCtx.receivedAt, reqCtx.assistantMessage, eventBus)

	// Generate title if needed
	if generateTitle && reqCtx.session.Title == "" {
		// Use the same model as the conversation for title generation
		modelID := ""
		if reqCtx.customAgent != nil && reqCtx.customAgent.Config.ModelID != "" {
			modelID = reqCtx.customAgent.Config.ModelID
		}
		logger.Infof(reqCtx.ctx, "Session has no title, starting async title generation, session ID: %s, model: %s", reqCtx.sessionID, modelID)
		h.sessionService.GenerateTitleAsync(asyncCtx, reqCtx.session, reqCtx.query, modelID, eventBus)
	}

	return streamCtx
}

// SearchKnowledge godoc
// @Summary      知识搜索
// @Description  在知识库中搜索（不使用LLM总结）。与产品内问答使用同一检索流程（召回、rerank、合并），外部检索首选；可覆盖召回参数与 rerank
// @Tags         问答
// @Accept       json
// @Produce      json
// @Param        request  body      SearchKnowledgeRequest  true  "搜索请求"
// @Param        resource_urls  query     string  false  "文件引用形式，public 返回可加载直链"  Enums(handle, public)  default(handle)
// @Success      200      {object}  map[string]interface{}  "搜索结果"
// @Failure      400      {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-search [post]
func (h *Handler) SearchKnowledge(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())
	logger.Info(ctx, "Start processing knowledge search request")

	// Parse request body
	var request SearchKnowledgeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Validate request parameters
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		c.Error(errors.NewBadRequestError("Query content cannot be empty"))
		return
	}

	// Resolve the storage-reference representation before retrieving, so a typo
	// or a rejected scope costs nothing.
	rewriter, err := h.resolveResourceRewriter(c)
	if err != nil {
		logger.Warnf(ctx, "Rejected resource URL mode: %v", err)
		_ = c.Error(err)
		return
	}

	// Merge single knowledge_base_id into knowledge_base_ids for backward compatibility
	knowledgeBaseIDs := request.KnowledgeBaseIDs
	if request.KnowledgeBaseID != "" {
		// Check if it's already in the list to avoid duplicates
		found := false
		for _, id := range knowledgeBaseIDs {
			if id == request.KnowledgeBaseID {
				found = true
				break
			}
		}
		if !found {
			knowledgeBaseIDs = append(knowledgeBaseIDs, request.KnowledgeBaseID)
		}
	}

	mentionScopes := tagScopesFromMentionedItems(request.MentionedItems)
	requestTagIDs := dedupRequestStrings(request.TagIDs)
	if err := validateUnscopedTagIDs(orphanTagIDsForScope(requestTagIDs, mentionScopes), secutils.SanitizeForLogArray(knowledgeBaseIDs)); err != nil {
		logger.Error(ctx, err.Error())
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}
	tagScopes := mergeTagScopesFromRequestIDs(mentionScopes, requestTagIDs, secutils.SanitizeForLogArray(knowledgeBaseIDs))

	if len(knowledgeBaseIDs) == 0 && len(request.KnowledgeIDs) == 0 && len(tagScopes) == 0 {
		logger.Error(ctx, "No knowledge base IDs, knowledge IDs, or tag scopes provided")
		c.Error(errors.NewBadRequestError("At least one knowledge_base_id, knowledge_base_ids, knowledge_ids, or scoped tag must be provided"))
		return
	}
	if err := types.AuthorizeTenantAPIKeyKnowledgeTargets(ctx, knowledgeBaseIDs, request.KnowledgeIDs); err != nil {
		c.Error(err)
		return
	}
	opts, err := knowledgeSearchOptions(&request)
	if err != nil {
		_ = c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	logger.Infof(
		ctx,
		"Knowledge search request, knowledge base IDs: %v, knowledge IDs: %v, tag scopes: %d, query: %s",
		secutils.SanitizeForLogArray(knowledgeBaseIDs),
		secutils.SanitizeForLogArray(request.KnowledgeIDs),
		len(tagScopes),
		secutils.SanitizeForLog(request.Query),
	)

	// Directly call knowledge retrieval service without LLM summarization
	retrieval, err := h.sessionService.SearchKnowledge(
		ctx, knowledgeBaseIDs, request.KnowledgeIDs, tagScopes, request.Query, opts,
	)
	if err != nil {
		// Typed AppErrors (e.g. an unknown rerank model_id) keep their code.
		if appErr, ok := errors.IsAppError(err); ok {
			_ = c.Error(appErr)
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(ctx, "Knowledge search completed, found %d results", len(retrieval.Results))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    rewriter.CopyReferences(ctx, retrieval.Results),
		"meta":    retrieval.Meta,
	})
}

// knowledgeSearchOptions validates the retrieval overrides of a
// knowledge-search request.
func knowledgeSearchOptions(request *SearchKnowledgeRequest) (*types.KnowledgeSearchOptions, error) {
	if request.MatchCount < 0 {
		return nil, fmt.Errorf("match_count must not be negative")
	}
	if request.MatchCount > types.MaxRequestedResults {
		return nil, fmt.Errorf("match_count must not exceed %d", types.MaxRequestedResults)
	}
	if request.DisableVectorMatch && request.DisableKeywordsMatch {
		return nil, fmt.Errorf("disable_vector_match and disable_keywords_match cannot both be true")
	}
	if err := request.Rerank.Validate(); err != nil {
		return nil, err
	}
	return &types.KnowledgeSearchOptions{
		VectorThreshold:      request.VectorThreshold,
		KeywordThreshold:     request.KeywordThreshold,
		MatchCount:           request.MatchCount,
		DisableKeywordsMatch: request.DisableKeywordsMatch,
		DisableVectorMatch:   request.DisableVectorMatch,
		Rerank:               request.Rerank,
	}, nil
}

// KnowledgeQA godoc
// @Summary      知识问答
// @Description  基于知识库的问答（使用LLM总结），支持SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Param        resource_urls  query     string  false  "文件引用形式，public 返回可加载直链"  Enums(handle, public)  default(handle)
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /knowledge-chat/{session_id} [post]
func (h *Handler) KnowledgeQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "KnowledgeQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Execute normal mode QA, generate title unless disabled
	if request.LocalBrowserEnabled {
		_ = c.Error(errors.NewBadRequestError("Local browser requests must use the agent endpoint"))
		return
	}
	h.executeQA(reqCtx, qaModeNormal, !request.DisableTitle)
}

// AgentQA godoc
// @Summary      Agent问答
// @Description  基于Agent的智能问答，支持多轮对话和SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Param        resource_urls  query     string  false  "文件引用形式，public 返回可加载直链"  Enums(handle, public)  default(handle)
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /agent-chat/{session_id} [post]
func (h *Handler) AgentQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "AgentQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Determine if agent mode should be enabled
	// Priority: customAgent.IsAgentMode() > request.AgentEnabled
	agentModeEnabled := request.AgentEnabled
	if reqCtx.customAgent != nil {
		agentModeEnabled = reqCtx.customAgent.IsAgentMode()
		logger.Infof(reqCtx.ctx, "Agent mode determined by custom agent: %v (config.agent_mode=%s)",
			agentModeEnabled, reqCtx.customAgent.Config.AgentMode)
	}

	// Sanity gate: agent mode requires a resolved CustomAgent. If we got
	// here with agent_enabled=true but agent_id missing/unresolvable, the
	// AgentQA service will fail deep inside the async goroutine with a
	// generic "custom agent configuration is required" error and the user
	// just sees a broken stream. Reject early with a clear 400 so the
	// frontend can recover (e.g. fall back to quick-answer). Most likely
	// cause is a stale localStorage settings blob where selectedAgentId
	// got blanked but isAgentEnabled stayed true — usually after a
	// cross-tenant switch where the previously selected agent is no
	// longer visible.
	if agentModeEnabled && reqCtx.customAgent == nil {
		logger.Warnf(reqCtx.ctx,
			"Agent mode requested without a resolvable agent_id, rejecting; session=%s, request.AgentID=%q",
			reqCtx.sessionID, secutils.SanitizeForLog(request.AgentID))
		c.Error(errors.NewBadRequestError(
			"agent_id is required when agent mode is enabled"))
		return
	}

	// Route to appropriate handler based on agent mode
	if agentModeEnabled {
		h.executeQA(reqCtx, qaModeAgent, true)
	} else {
		logger.Infof(reqCtx.ctx, "Agent mode disabled, delegating to normal mode for session: %s", reqCtx.sessionID)
		h.executeQA(reqCtx, qaModeNormal, !request.DisableTitle)
	}
}

// qaMode determines which QA execution path to use.
type qaMode int

const (
	qaModeNormal qaMode = iota // KnowledgeQA pipeline (RAG / pure chat)
	qaModeAgent                // Agent engine with tool calling
)

// persistTurnMessages writes the user and assistant rows for this turn.
// Follow-up handoff calls it before SetLiveRun so POST /steer can verify the
// new assistant instead of seeing an empty session. executeQA skips work that
// is already done when those IDs are populated.
func (h *Handler) persistTurnMessages(ctx context.Context, reqCtx *qaRequestContext) error {
	createdUser := false
	if reqCtx.userMessageID == "" {
		userMessageAttachments := reqCtx.attachments
		if len(reqCtx.attachmentMetas) > 0 {
			userMessageAttachments = append(
				append(types.MessageAttachments{}, reqCtx.attachments...),
				reqCtx.attachmentMetas...,
			)
		}
		userMsg, err := h.createUserMessage(
			ctx,
			reqCtx.sessionID,
			reqCtx.userInput,
			reqCtx.requestID,
			reqCtx.mentionedItems,
			convertImageAttachments(reqCtx.images),
			userMessageAttachments,
			reqCtx.channel,
			reqCtx.suggestionAttribution,
		)
		if err != nil {
			return err
		}
		reqCtx.userMessageID = userMsg.ID
		reqCtx.userCreatedAt = userMsg.CreatedAt
		createdUser = true
	}
	if reqCtx.assistantMessage == nil {
		reqCtx.assistantMessage = &types.Message{
			SessionID:   reqCtx.sessionID,
			Role:        "assistant",
			IsCompleted: false,
			RequestID:   reqCtx.requestID,
			CreatedAt:   time.Now(),
		}
	}
	if reqCtx.assistantMessage.ID == "" {
		assistantMessagePtr, err := h.createAssistantMessage(ctx, reqCtx.assistantMessage)
		if err != nil {
			h.rollbackTurnMessages(ctx, reqCtx, createdUser, false)
			return err
		}
		reqCtx.assistantMessage = assistantMessagePtr
	}
	return nil
}

// rollbackTurnMessages deletes user/assistant rows this request just created
// so a failed SetLiveRun / ClaimLiveRun cannot leave an orphan turn in history.
func (h *Handler) rollbackTurnMessages(ctx context.Context, reqCtx *qaRequestContext, user, assistant bool) {
	if h.messageService == nil || reqCtx == nil {
		return
	}
	sessionID := reqCtx.sessionID
	if user && reqCtx.userMessageID != "" {
		if err := h.messageService.DeleteMessage(ctx, sessionID, reqCtx.userMessageID); err != nil {
			logger.Warnf(ctx, "turn rollback failed for user message %s: %v", reqCtx.userMessageID, err)
		} else {
			reqCtx.userMessageID = ""
		}
	}
	if assistant && reqCtx.assistantMessage != nil && reqCtx.assistantMessage.ID != "" {
		if err := h.messageService.DeleteMessage(ctx, sessionID, reqCtx.assistantMessage.ID); err != nil {
			logger.Warnf(ctx, "turn rollback failed for assistant message %s: %v", reqCtx.assistantMessage.ID, err)
		} else {
			reqCtx.assistantMessage.ID = ""
		}
	}
}

func (h *Handler) rejectIfOtherAgentRunLive(ctx context.Context, reqCtx *qaRequestContext) error {
	liveID, _, err := h.streamManager.GetLiveRun(ctx, reqCtx.sessionID)
	if err != nil {
		return errors.NewServiceUnavailableError("Failed to look up running turn")
	}
	if liveID == "" {
		return nil
	}
	self := ""
	if reqCtx.assistantMessage != nil {
		self = reqCtx.assistantMessage.ID
	}
	if liveID == self {
		return nil
	}
	return errors.NewConflictError("another turn is already running in this session")
}

func (h *Handler) rejectIfSessionRewinding(ctx context.Context, sessionID string) error {
	type rewindSendGuard interface {
		RejectSendIfRewinding(context.Context, string) error
	}
	guard, ok := h.sessionService.(rewindSendGuard)
	if !ok {
		return nil
	}
	err := guard.RejectSendIfRewinding(ctx, sessionID)
	if err == nil {
		return nil
	}
	if stderrors.Is(err, sandbox.ErrSessionRewindLocked) {
		return errors.NewConflictError("session rewind is in progress")
	}
	return errors.NewInternalServerError(err.Error())
}

func (h *Handler) holdQATurn(ctx context.Context, reqCtx *qaRequestContext) (func(), bool) {
	noop := func() {}
	if reqCtx == nil {
		return noop, true
	}
	type sessionTurnHolder interface {
		HoldSandboxTurn(context.Context, string, string) (func(), error)
	}
	holder, ok := h.sessionService.(sessionTurnHolder)
	if !ok {
		return noop, true
	}
	configID := ""
	if reqCtx.customAgent != nil {
		configID = reqCtx.customAgent.Config.SandboxConfigID
	}
	release, err := holder.HoldSandboxTurn(ctx, reqCtx.sessionID, configID)
	if err == nil {
		// The QA service reads this off the built request and skips its own
		// hold, so the send path opens and closes the lease exactly once.
		reqCtx.turnLeaseHeld = true
	}
	if err != nil {
		if reqCtx.c != nil && !reqCtx.skipSSE {
			if stderrors.Is(err, sandbox.ErrSessionRewindLocked) || stderrors.Is(err, sandbox.ErrSessionTurnActive) {
				_ = reqCtx.c.Error(errors.NewConflictError("session rewind is in progress"))
			} else {
				_ = reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
			}
		} else {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{"session_id": reqCtx.sessionID})
		}
		return noop, false
	}
	if release == nil {
		release = noop
	}
	var once sync.Once
	return func() { once.Do(release) }, true
}

// executeQA is the unified execution flow for both KnowledgeQA and AgentQA modes.
// It handles message creation, SSE setup, VLM analysis, service invocation, and error handling.
func (h *Handler) executeQA(reqCtx *qaRequestContext, mode qaMode, generateTitle bool) {
	ctx := reqCtx.ctx
	sessionID := reqCtx.sessionID

	// Persist the input-bar state used for this request so reopening the
	// session can rehydrate agent / model / KB / web-search / MCP selections.
	// This is a pure UI memo (no behavioural effect) and runs in a goroutine
	// to avoid adding a DB round-trip to TTFB. Use WithoutCancel so a fast
	// client disconnect doesn't drop the write.
	go h.persistLastRequestState(ctx, reqCtx, mode)

	if mode == qaModeAgent {
		if err := h.rejectIfOtherAgentRunLive(ctx, reqCtx); err != nil {
			if reqCtx.c != nil && !reqCtx.skipSSE {
				_ = reqCtx.c.Error(err)
			} else {
				logger.ErrorWithFields(ctx, err, map[string]interface{}{"session_id": sessionID})
			}
			return
		}
	}
	if err := h.rejectIfSessionRewinding(ctx, sessionID); err != nil {
		if reqCtx.c != nil && !reqCtx.skipSSE {
			_ = reqCtx.c.Error(err)
		} else {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{"session_id": sessionID})
		}
		return
	}

	releaseQATurn, held := h.holdQATurn(ctx, reqCtx)
	if !held {
		return
	}

	// Agent mode: emit agent query event before message creation
	if mode == qaModeAgent {
		if err := event.Emit(ctx, event.Event{
			Type:      event.EventAgentQuery,
			SessionID: sessionID,
			RequestID: reqCtx.requestID,
			Data: event.AgentQueryData{
				SessionID: sessionID,
				Query:     reqCtx.query,
				RequestID: reqCtx.requestID,
			},
		}); err != nil {
			releaseQATurn()
			logger.Errorf(ctx, "Failed to emit agent query event: %v", err)
			return
		}
	}

	createdUser := reqCtx.userMessageID == ""
	createdAssistant := reqCtx.assistantMessage == nil || reqCtx.assistantMessage.ID == ""

	// Create user message. Include pre-uploaded document metadata so history
	// reload shows the attachments even though their content is selected later.
	if err := h.persistTurnMessages(ctx, reqCtx); err != nil {
		releaseQATurn()
		if reqCtx.c != nil && !reqCtx.skipSSE {
			_ = reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		} else {
			logger.ErrorWithFields(ctx, err, map[string]interface{}{"session_id": sessionID})
		}
		return
	}

	if mode == qaModeNormal {
		logger.Infof(ctx, "Using knowledge bases: %v", reqCtx.knowledgeBaseIDs)
	} else {
		logger.Infof(ctx, "Calling agent QA service, session ID: %s", sessionID)
	}

	// Move the previous run's leftovers onto this run's queue before the run
	// announces itself as live. The other order leaves a window where a
	// client that reloads the queue sees an empty one and drops messages the
	// user can still see in the composer.
	if len(reqCtx.steerCarryOver) > 0 && reqCtx.assistantMessage != nil {
		if err := h.streamManager.AppendSteerEvents(
			ctx, sessionID, reqCtx.assistantMessage.ID, reqCtx.steerCarryOver,
		); err != nil {
			logger.Warnf(ctx, "steer carry-over append failed for session %s: %v", sessionID, err)
		}
	}

	// Setup SSE stream
	streamCtx := h.setupSSEStream(reqCtx, generateTitle, mode)
	streamCtx.releaseTurn = releaseQATurn
	if streamCtx.liveRunFailed {
		if streamCtx.cancel != nil {
			streamCtx.cancel()
		}
		releaseQATurn()
		h.rollbackTurnMessages(ctx, reqCtx, createdUser, createdAssistant)
		if reqCtx.c != nil && !reqCtx.skipSSE {
			if stderrors.Is(streamCtx.liveRunErr, stream.ErrLiveRunExists) {
				_ = reqCtx.c.Error(errors.NewConflictError("another turn is already running in this session"))
			} else {
				_ = reqCtx.c.Error(errors.NewServiceUnavailableError("Failed to publish running turn"))
			}
		}
		return
	}

	// The turn is live in this process from here until it reaches a terminal
	// state: shutdown can then end it as interrupted, and the reconciler
	// knows not to touch a row this process is still generating.
	h.trackTurn(reqCtx, streamCtx)

	// Normal mode: register completion handler on EventAgentFinalAnswer
	// (Agent mode handles completion in the defer block instead)
	if mode == qaModeNormal {
		h.watchQuickAnswerTurn(streamCtx, reqCtx)
	}

	// Execute QA asynchronously
	asyncDone := make(chan struct{})
	go func() {
		defer close(asyncDone)
		h.runTurn(reqCtx, streamCtx, mode)
	}()

	if reqCtx.skipSSE {
		<-asyncDone
		return
	}

	// Handle SSE events (blocking)
	shouldWaitForTitle := generateTitle && reqCtx.session.Title == ""
	h.handleAgentEventsForSSE(ctx, reqCtx.c, sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, streamCtx.eventBus, shouldWaitForTitle, reqCtx.resourceRewriter)
}

// trackTurn indexes the turn as generating in this process until it reaches
// a terminal state (see Handler.turns). Shutdown then ends it as interrupted
// through the same finalizeTurn every other ending uses.
func (h *Handler) trackTurn(reqCtx *qaRequestContext, streamCtx *sseStreamContext) {
	if h.turns == nil {
		return
	}
	streamCtx.unregisterTurn = h.turns.Register(
		reqCtx.sessionID, reqCtx.assistantMessage.ID,
		func(ctx context.Context) { h.finalizeTurn(ctx, streamCtx, interruptedOutcome(ctx)) },
		func() {
			// Shutdown cancels before it writes `interrupted`; the flag keeps
			// the run's own teardown from recording that as a user stop.
			streamCtx.shutdownInterrupted.Store(true)
			if streamCtx.cancel != nil {
				streamCtx.cancel()
			}
		},
	)
}

// watchQuickAnswerTurn registers the event handlers that end a quick-answer
// turn from its stream: the final answer chunk completes it, a stream-level
// error fails it.
func (h *Handler) watchQuickAnswerTurn(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	var completionHandled bool

	// Persist the pipeline's retrieval/attachment stages so a reloaded
	// conversation redraws the timeline it showed while streaming, including
	// turns that searched and cited nothing.
	registerQuickAnswerTimelineRecorder(streamCtx.eventBus, streamCtx.assistantMessage)

	// Persist reasoning_content into agent_steps so historical reload can
	// reconstruct the thinking card (same shape as Agent-mode steps).
	// Accumulate on assistantMessage directly so user-initiated stop also
	// keeps whatever reasoning had streamed before the cancel.
	streamCtx.eventBus.On(event.EventAgentThought, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentThoughtData)
		if !ok || data.Content == "" {
			return nil
		}
		appendQuickAnswerReasoning(streamCtx.assistantMessage, data.Content)
		return nil
	})

	streamCtx.eventBus.On(event.EventAgentFinalAnswer, func(_ context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.AgentFinalAnswerData)
		if !ok {
			return nil
		}
		streamCtx.assistantMessage.Content += data.Content
		if data.IsFallback {
			streamCtx.assistantMessage.IsFallback = true
		}
		if data.Truncated {
			markQuickAnswerTruncated(streamCtx.assistantMessage)
		}
		if data.Done {
			if completionHandled {
				return nil
			}
			completionHandled = true

			logger.Infof(streamCtx.asyncCtx, "Knowledge QA service completed for session: %s", reqCtx.sessionID)
			updateCtx := context.WithValue(streamCtx.asyncCtx, types.TenantIDContextKey, reqCtx.session.TenantID)
			h.completeQuickAnswerTurn(updateCtx, streamCtx, reqCtx.query, reqCtx.userMessageID)
		}
		return nil
	})
}

// runTurn drives one assistant turn to its end: attachment resolution, the
// QA service call, and every way the turn can finish (BF-03 §4.2). It runs in
// its own goroutine from executeQA and is kept free of gin so the endings can
// be tested directly.
//
// How each ending writes its terminal state:
//   - quick-answer success: the EventAgentFinalAnswer handler → completed;
//   - agent success: the defer below → completed;
//   - user stop: the stop handler → stopped (a cancelled agent run persists
//     `stopped` again here, which changes nothing);
//   - agent engine error: the engine's error event → watchTurnErrors →
//     failed (before the client sees the frame); the defer then persists the
//     steps without completing the turn and drops queued follow-ups;
//   - service error: finalizeTurn → failed, then the error event (once:
//     not again when the engine already emitted it);
//   - panic: finalizeTurn → interrupted before anything else persists;
//   - agent mode backstop: finalizeTurn → interrupted, which is a no-op when
//     the turn already ended. KnowledgeQA returns while its stream is still
//     running, so no such backstop exists for quick answers: their stream
//     errors are caught by watchTurnErrors and anything else is
//     left to the reconciler.
func (h *Handler) runTurn(reqCtx *qaRequestContext, streamCtx *sseStreamContext, mode qaMode) {
	sessionID := reqCtx.sessionID
	defer func() {
		panicked := false
		if r := recover(); r != nil {
			panicked = true
			if streamCtx.releaseTurn != nil {
				streamCtx.releaseTurn()
			}
			buf := make([]byte, 10240)
			runtime.Stack(buf, true)
			stageName := "Knowledge QA"
			if mode == qaModeAgent {
				stageName = "Agent QA"
			}
			logger.ErrorWithFields(streamCtx.asyncCtx,
				errors.NewInternalServerError(fmt.Sprintf("%s service panicked: %v\n%s", stageName, r, string(buf))),
				map[string]interface{}{"session_id": sessionID})
			// No completion path ran or will run for this turn. Record
			// the loss before anything else can persist a default state.
			h.finalizeTurn(streamCtx.asyncCtx, streamCtx, interruptedOutcome(streamCtx.asyncCtx))
		}
		// Agent mode: complete the assistant message in defer (normal mode does it via event handler)
		if mode == qaModeAgent {
			if streamCtx.releaseTurn != nil {
				streamCtx.releaseTurn()
			}
			// Use WithoutCancel so a user-triggered stop (which cancels
			// asyncCtx) doesn't also cancel the GORM UPDATE that persists
			// AgentSteps/Content. Without this, cancelled-ctx makes
			// GORM skip the write and the agent's intermediate steps
			// (thinking / tool_call history) are lost on page refresh.
			updateCtx := context.WithValue(
				context.WithoutCancel(streamCtx.asyncCtx),
				types.TenantIDContextKey, reqCtx.session.TenantID,
			)
			// A cancelled run is a user stop (or a shutdown interrupt,
			// which already wrote `interrupted` and is kept as is).
			outcome := types.MessageStatusCompleted
			if streamCtx.asyncCtx.Err() != nil {
				outcome = types.MessageStatusStopped
			}
			if streamCtx.shutdownInterrupted.Load() {
				// Shutdown cancelled the run and writes `interrupted` itself;
				// record it here too so the persist below keeps that state
				// (whichever lands first, the other matches it).
				h.finalizeTurn(updateCtx, streamCtx, interruptedOutcome(updateCtx))
			}

			// Claim any follow-up before completing this row. liveAgentRun
			// treats a completed assistant as idle and would ClearLiveRun
			// the old marker — exactly the new_run window. The follow-up
			// SetLiveRun's first; this run's CAS ClearLiveRun then no-ops.
			if outcome == types.MessageStatusStopped || panicked || streamCtx.turnEndedWithoutAnswer() {
				// The turn was cancelled (a user stop), failed or panicked.
				// Queued follow-ups are dropped rather than fired off as a
				// brand-new run the user did not ask for: after a stop that
				// is what stopping means, and after a failure the follow-up
				// would build on an answer that does not exist. A failed
				// turn keeps its state; the persist below only stores its
				// steps and partial content.
				injected := map[string]struct{}{}
				if streamCtx.steerSink != nil {
					injected = streamCtx.steerSink.InjectedIDs()
				}
				h.discardSteerBacklog(updateCtx, sessionID, streamCtx.assistantMessage.ID, injected)
				h.completeStreamAssistantMessage(updateCtx, streamCtx, reqCtx.query, reqCtx.userMessageID, outcome)
			} else {
				kicked := h.kickNextRunFromSteerBacklog(updateCtx, reqCtx, streamCtx)
				h.completeStreamAssistantMessage(updateCtx, streamCtx, reqCtx.query, reqCtx.userMessageID, outcome)
				// A /steer that landed while we were completing still sits
				// on this run. Claim it before ClearLiveRun so it is not
				// stranded on a list nobody will drain.
				if !kicked {
					h.kickNextRunFromSteerBacklog(updateCtx, reqCtx, streamCtx)
				}
			}
			if err := h.streamManager.ClearLiveRun(
				updateCtx, sessionID, streamCtx.assistantMessage.ID,
			); err != nil {
				logger.Warnf(updateCtx, "live run cleanup failed for session %s: %v", sessionID, err)
			}
			logger.Infof(streamCtx.asyncCtx, "Agent QA service completed for session: %s", sessionID)
			// Backstop: this goroutine's exit is the end of an agent
			// turn. If every path above failed to write a terminal state
			// (persistence unavailable, a missed branch), leave the row
			// interrupted instead of generating forever. The write is
			// conditional, so a turn that already ended is untouched.
			h.finalizeTurn(updateCtx, streamCtx, interruptedOutcome(updateCtx))
		}
		// Knowledge mode normally releases on EventAgentFinalAnswer Done.
		// If that event never fires, release here so rewind is not blocked
		// until the Redis turn TTL expires.
		if mode == qaModeNormal && streamCtx.releaseTurn != nil {
			streamCtx.releaseTurn()
		}
	}()

	// Resolve pre-uploaded attachments (may still be parsing): waits with a
	// timeline step so the send is not blocked, then injects content/images.
	if mode == qaModeAgent {
		h.describeTemporaryAttachmentsForAgent(streamCtx, reqCtx)
	} else {
		h.resolveTemporaryAttachments(streamCtx, reqCtx)
	}

	// Run VLM image analysis if applicable
	h.runVLMAnalysisIfNeeded(streamCtx, reqCtx, mode)

	// Build QA request and invoke the appropriate service
	qaReq := reqCtx.buildQARequest()

	var serviceErr error
	var stageName string
	if mode == qaModeNormal {
		stageName = stageKnowledgeQA
		serviceErr = h.sessionService.KnowledgeQA(streamCtx.asyncCtx, qaReq, streamCtx.eventBus)
	} else {
		stageName = stageAgentExecution
		serviceErr = h.sessionService.AgentQA(streamCtx.asyncCtx, qaReq, streamCtx.eventBus)
	}

	if serviceErr != nil {
		if mode == qaModeNormal && streamCtx.releaseTurn != nil {
			streamCtx.releaseTurn()
		}
		// A user-requested stop cancels asyncCtx, which surfaces here as a
		// context cancellation. That is an expected outcome, not a failure:
		// the stop handler already wrote `stopped` and the stop event
		// notifies the client, so don't emit a spurious error event (which
		// would otherwise show an error toast).
		switch {
		case streamCtx.asyncCtx.Err() != nil:
			logger.Infof(streamCtx.asyncCtx, "QA cancelled by user stop for session: %s", sessionID)
		case streamCtx.turnEndedWithoutAnswer():
			// The engine already emitted the error event for this failure and
			// watchTurnErrors stored it; a second event would show twice.
			logger.ErrorWithFields(streamCtx.asyncCtx, serviceErr, nil)
		default:
			logger.ErrorWithFields(streamCtx.asyncCtx, serviceErr, nil)
			// The turn is over: write its terminal state before telling
			// the client, so a reload right after the error never sees a
			// message that is still "generating" (BF-03 §4.2).
			outcome := failureOutcome(streamCtx.asyncCtx, stageName, serviceErr)
			h.finalizeTurn(streamCtx.asyncCtx, streamCtx, outcome)
			if err := streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
				Type:      event.EventError,
				SessionID: sessionID,
				Data: event.ErrorData{
					Error:     serviceErr.Error(),
					ErrorCode: outcome.code,
					Stage:     stageName,
					SessionID: sessionID,
				},
			}); err != nil {
				// The terminal state is already stored; only the live
				// notification was lost, and a reload shows the failure.
				logger.Warnf(streamCtx.asyncCtx, "error event for session %s not delivered: %v", sessionID, err)
			}
		}
	}
}

// watchTurnErrors ends a turn whose generation reported an error event. Every
// error event ends the turn it belongs to: the agent engine emits one when
// its run fails, and a quick-answer stream reports model failures from its
// own goroutine after KnowledgeQA has already returned nil, where the
// service-error branch in runTurn never sees them (BF-03 §4.2).
func (h *Handler) watchTurnErrors(streamCtx *sseStreamContext) {
	streamCtx.eventBus.On(event.EventError, func(ctx context.Context, evt event.Event) error {
		data, ok := evt.Data.(event.ErrorData)
		if !ok {
			return nil
		}
		// A cancelled run is a stop or a shutdown; each writes its own
		// state, and the engine's cancellation error is not a failure.
		if streamCtx.asyncCtx != nil && streamCtx.asyncCtx.Err() != nil {
			return nil
		}
		// The service-error branch in runTurn and the persist-failure path
		// end the turn before they emit; nothing is left to do for those.
		outcome := failureOutcome(ctx, data.Stage, stderrors.New(data.Error))
		if data.ErrorCode != "" {
			outcome.code = data.ErrorCode
			outcome.message = types.TurnFailedMessage(data.ErrorCode, types.LanguageFromContextOrDefault(ctx))
		}
		h.finalizeTurn(ctx, streamCtx, outcome)
		return nil
	})
}

// runVLMAnalysisIfNeeded runs VLM image analysis within the async goroutine,
// emitting tool_call/tool_result events so the user can see progress.
// For normal mode, VLM only runs on the pure-chat path (no KB, no web search);
// RAG paths defer VLM to the pipeline rewrite step.
// For agent mode, VLM always runs when images and a VLM model are present.
func (h *Handler) runVLMAnalysisIfNeeded(streamCtx *sseStreamContext, reqCtx *qaRequestContext, mode qaMode) {
	if len(reqCtx.images) == 0 || reqCtx.customAgent == nil || reqCtx.customAgent.Config.VLMModelID == "" {
		return
	}

	sessionID := reqCtx.sessionID

	// In normal mode, only run VLM for pure-chat path
	if mode == qaModeNormal {
		hasRequestKBs := len(reqCtx.knowledgeBaseIDs) > 0 || len(reqCtx.knowledgeIDs) > 0
		agentWillResolveKBs := false
		if !hasRequestKBs && reqCtx.customAgent != nil && !reqCtx.customAgent.Config.RetrieveKBOnlyWhenMentioned {
			switch reqCtx.customAgent.Config.KBSelectionMode {
			case "all":
				agentWillResolveKBs = true
			case "selected", "":
				agentWillResolveKBs = len(reqCtx.customAgent.Config.KnowledgeBases) > 0
			case "none":
				agentWillResolveKBs = false
			default:
				agentWillResolveKBs = len(reqCtx.customAgent.Config.KnowledgeBases) > 0
			}
		}
		if hasRequestKBs || agentWillResolveKBs || reqCtx.webSearchEnabled {
			return // VLM will be handled by the pipeline rewrite step
		}
	}

	// Emit VLM tool call/result events
	toolCallID := uuid.New().String()
	iteration := 0 // agent mode uses iteration field

	streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
		Type:      event.EventAgentToolCall,
		SessionID: sessionID,
		Data: event.AgentToolCallData{
			ToolCallID: toolCallID,
			ToolName:   "image_analysis",
			Iteration:  iteration,
		},
	})

	vlmStart := time.Now()
	h.analyzeImageAttachments(streamCtx.asyncCtx, reqCtx.images,
		reqCtx.customAgent.Config.VLMModelID, reqCtx.query)

	outputMsg := "已分析图片内容"
	if mode == qaModeAgent {
		outputMsg = "已查看图片内容"
	}
	streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
		Type:      event.EventAgentToolResult,
		SessionID: sessionID,
		Data: event.AgentToolResultData{
			ToolCallID: toolCallID,
			ToolName:   "image_analysis",
			Output:     outputMsg,
			Success:    true,
			Duration:   time.Since(vlmStart).Milliseconds(),
			Iteration:  iteration,
		},
	})
}

// defaultAttachmentParseWaitTimeout bounds how long a QA turn waits for
// still-parsing attachments before proceeding with only the finished ones.
// Large or scanned documents can exceed this; raise it via
// PRIMIND_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC when needed.
const defaultAttachmentParseWaitTimeout = 60 * time.Second

// attachmentParseWaitTimeout returns the configured wait timeout, honoring the
// PRIMIND_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC override (in seconds) and falling
// back to the default when unset or invalid.
func attachmentParseWaitTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("PRIMIND_CHAT_ATTACHMENT_WAIT_TIMEOUT_SEC")); raw != "" {
		if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultAttachmentParseWaitTimeout
}

// resolveTemporaryAttachments selects prompt content for pre-uploaded documents
// after the SSE stream is live. When any attachment is still parsing it emits a
// "attachment_parsing" timeline step and waits (bounded); unfinished attachments
// are skipped rather than blocking or failing the whole turn.
func (h *Handler) resolveTemporaryAttachments(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	if len(reqCtx.attachmentIDs) == 0 {
		return
	}
	ctx := streamCtx.asyncCtx
	sessionID := reqCtx.sessionID
	// Prefer the session's tenant over gin.Context: this runs in an async
	// goroutine after the HTTP handler may have returned.
	tenantID := reqCtx.session.TenantID

	start := time.Now()
	var toolCallID string
	if h.hasPendingAttachments(ctx, tenantID, sessionID, reqCtx.attachmentIDs) {
		toolCallID = uuid.New().String()
		streamCtx.eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentToolCall,
			SessionID: sessionID,
			Data: event.AgentToolCallData{
				ToolCallID: toolCallID,
				ToolName:   "attachment_parsing",
				Iteration:  0,
			},
		})
		waitTimeout := attachmentParseWaitTimeout()
		if reqCtx.customAgent != nil && reqCtx.customAgent.Config.AttachmentParseWaitTimeoutSec > 0 {
			waitTimeout = time.Duration(reqCtx.customAgent.Config.AttachmentParseWaitTimeoutSec) * time.Second
		}
		h.waitForAttachments(ctx, tenantID, sessionID, reqCtx.attachmentIDs, waitTimeout)
	}

	readyIDs, skipped := h.partitionReadyAttachments(ctx, tenantID, sessionID, reqCtx.attachmentIDs)

	var temporaryResult *types.TemporaryDocumentPromptResult
	var resolveErr error
	if len(readyIDs) > 0 {
		temporaryResult, resolveErr = h.temporaryDocuments.ResolveForPrompt(ctx, tenantID, sessionID, readyIDs, reqCtx.query)
	}

	if toolCallID != "" {
		output := fmt.Sprintf("已解析 %d 个附件", len(readyIDs))
		if skipped > 0 {
			output += fmt.Sprintf("，%d 个未完成已跳过", skipped)
		}
		success := resolveErr == nil
		if resolveErr != nil {
			output = fmt.Sprintf("附件解析失败: %v", resolveErr)
		}
		streamCtx.eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentToolResult,
			SessionID: sessionID,
			Data: event.AgentToolResultData{
				ToolCallID: toolCallID,
				ToolName:   "attachment_parsing",
				Output:     output,
				Success:    success,
				Duration:   time.Since(start).Milliseconds(),
				Iteration:  0,
				Data: map[string]interface{}{
					"display_type":  "attachment_parsing",
					"parsed_count":  len(readyIDs),
					"skipped_count": skipped,
				},
			},
		})
	}
	if resolveErr != nil || temporaryResult == nil {
		if resolveErr != nil {
			logger.Warnf(ctx, "temporary attachment resolution failed for session %s: %v", sessionID, resolveErr)
		}
		return
	}

	h.applyTemporaryAttachments(ctx, reqCtx, temporaryResult)
}

// describeTemporaryAttachmentsForAgent hands an agent turn a manifest of the
// pre-uploaded documents without waiting for parsing to finish or choosing
// content by the question. The agent reads attachments itself through
// read_file's attachment:// paths, which wait for a document that is still
// parsing only when the agent actually needs it.
func (h *Handler) describeTemporaryAttachmentsForAgent(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	if len(reqCtx.attachmentIDs) == 0 {
		return
	}
	ctx := streamCtx.asyncCtx
	h.awaitImageAttachmentsForAgent(streamCtx, reqCtx)
	result, err := h.temporaryDocuments.DescribeForAgent(
		ctx, reqCtx.session.TenantID, reqCtx.sessionID, reqCtx.attachmentIDs,
	)
	if err != nil || result == nil {
		if err != nil {
			logger.Warnf(ctx, "temporary attachment manifest failed for session %s: %v", reqCtx.sessionID, err)
		}
		return
	}
	h.applyTemporaryAttachments(ctx, reqCtx, result)
}

// awaitImageAttachmentsForAgent waits for standalone image uploads. Document
// images are read on demand through read_file.
func (h *Handler) awaitImageAttachmentsForAgent(streamCtx *sseStreamContext, reqCtx *qaRequestContext) {
	if reqCtx.customAgent == nil || !reqCtx.customAgent.Config.ImageUploadEnabled {
		return
	}
	var imageIDs []string
	for _, meta := range reqCtx.attachmentMetas {
		if meta.ID != "" && docparser.IsImageFormat(meta.FileType) {
			imageIDs = append(imageIDs, meta.ID)
		}
	}
	ctx := streamCtx.asyncCtx
	tenantID := reqCtx.session.TenantID
	if len(imageIDs) == 0 || !h.hasPendingAttachments(ctx, tenantID, reqCtx.sessionID, imageIDs) {
		return
	}
	start := time.Now()
	toolCallID := uuid.New().String()
	_ = streamCtx.eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentToolCall,
		SessionID: reqCtx.sessionID,
		Data:      event.AgentToolCallData{ToolCallID: toolCallID, ToolName: "attachment_parsing"},
	})
	waitTimeout := attachmentParseWaitTimeout()
	if reqCtx.customAgent.Config.AttachmentParseWaitTimeoutSec > 0 {
		waitTimeout = time.Duration(reqCtx.customAgent.Config.AttachmentParseWaitTimeoutSec) * time.Second
	}
	h.waitForAttachments(ctx, tenantID, reqCtx.sessionID, imageIDs, waitTimeout)
	ready, skipped := h.partitionReadyAttachments(ctx, tenantID, reqCtx.sessionID, imageIDs)
	output := fmt.Sprintf("已解析 %d 个附件", len(ready))
	if skipped > 0 {
		output += fmt.Sprintf("，%d 个未完成已跳过", skipped)
	}
	_ = streamCtx.eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentToolResult,
		SessionID: reqCtx.sessionID,
		Data: event.AgentToolResultData{
			ToolCallID: toolCallID,
			ToolName:   "attachment_parsing",
			Output:     output,
			Success:    true,
			Duration:   time.Since(start).Milliseconds(),
			Data: map[string]interface{}{
				"display_type":  "attachment_parsing",
				"parsed_count":  len(ready),
				"skipped_count": skipped,
			},
		},
	})
}

// applyTemporaryAttachments filters resolved attachments by the agent's
// supported file types, adds them (and their images) to the turn, and stores
// the selected content on the user message for later history replay.
func (h *Handler) applyTemporaryAttachments(
	ctx context.Context, reqCtx *qaRequestContext, temporaryResult *types.TemporaryDocumentPromptResult,
) {
	attachments := temporaryResult.Attachments
	if reqCtx.customAgent != nil && len(reqCtx.customAgent.Config.SupportedFileTypes) > 0 {
		filtered := attachments[:0]
		for _, att := range attachments {
			ext := strings.TrimPrefix(strings.ToLower(att.FileType), ".")
			if containsFileType(reqCtx.customAgent.Config.SupportedFileTypes, ext) {
				filtered = append(filtered, att)
			}
		}
		attachments = filtered
	}
	reqCtx.attachments = append(reqCtx.attachments, attachments...)
	// Persist the freshly selected content back onto the stored user message.
	// The message was created with metadata-only attachment entries (content is
	// selected here, after the SSE stream is live), so without this write a
	// later Agent-mode turn rebuilds history from the Attachments column and
	// sees empty attachments (see buildUserHistoryMessage in agent_history.go).
	h.persistResolvedAttachmentContent(ctx, reqCtx, attachments)
	if reqCtx.customAgent != nil && reqCtx.customAgent.Config.ImageUploadEnabled {
		for _, imageURL := range temporaryResult.ImageURLs {
			reqCtx.images = append(reqCtx.images, ImageAttachment{URL: imageURL})
		}
	}
}

// persistResolvedAttachmentContent writes the parsed content of pre-uploaded
// attachments back onto the stored user message so multi-turn history can
// replay it. Entries are matched by their temporary-document ID; only those
// present on the message are enriched. Failures are logged but never bubble up
// — losing the write only degrades follow-up context, it must not fail the turn.
func (h *Handler) persistResolvedAttachmentContent(
	ctx context.Context, reqCtx *qaRequestContext, resolved types.MessageAttachments,
) {
	if reqCtx.userMessageID == "" || len(resolved) == 0 {
		return
	}
	// Detach from the request/stream lifetime and pin the session tenant so a
	// user-triggered stop (which cancels asyncCtx) does not drop the write.
	updateCtx := context.WithValue(
		context.WithoutCancel(ctx), types.TenantIDContextKey, reqCtx.session.TenantID,
	)
	msg, err := h.messageService.GetMessage(updateCtx, reqCtx.sessionID, reqCtx.userMessageID)
	if err != nil || msg == nil {
		logger.Warnf(updateCtx, "persist attachment content: load user message %s failed: %v",
			reqCtx.userMessageID, err)
		return
	}
	byID := make(map[string]types.MessageAttachment, len(resolved))
	for _, att := range resolved {
		if att.ID != "" {
			byID[att.ID] = att
		}
	}
	changed := false
	for i := range msg.Attachments {
		if msg.Attachments[i].ID == "" {
			continue
		}
		if enriched, ok := byID[msg.Attachments[i].ID]; ok {
			msg.Attachments[i] = enriched
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := h.messageService.UpdateMessage(updateCtx, msg); err != nil {
		logger.Warnf(updateCtx, "persist attachment content: update user message %s failed: %v",
			reqCtx.userMessageID, err)
	}
}

// normalizeTemporaryAttachmentIDs rejects oversized ID lists before any DB
// lookup, then returns a deduplicated, order-preserving list of non-empty IDs.
func normalizeTemporaryAttachmentIDs(ids []string) ([]string, error) {
	if len(ids) > types.MaxTemporaryAttachmentsPerMessage {
		return nil, fmt.Errorf("a message can use at most %d attachments", types.MaxTemporaryAttachmentsPerMessage)
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// hasPendingAttachments reports whether any of the given documents is still
// uploading or being parsed.
func (h *Handler) hasPendingAttachments(ctx context.Context, tenantID uint64, sessionID string, ids []string) bool {
	for _, id := range ids {
		doc, err := h.temporaryDocuments.Get(ctx, tenantID, sessionID, id)
		if err != nil || doc == nil {
			continue
		}
		if doc.Status == types.TemporaryDocumentStatusUploaded ||
			doc.Status == types.TemporaryDocumentStatusProcessing {
			return true
		}
	}
	return false
}

// waitForAttachments polls until no attachment is pending or the timeout / ctx
// cancellation fires.
func (h *Handler) waitForAttachments(ctx context.Context, tenantID uint64, sessionID string, ids []string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !h.hasPendingAttachments(ctx, tenantID, sessionID, ids) || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// partitionReadyAttachments splits the ids into ready ones and a count of those
// skipped (missing, failed, or still parsing after the wait).
func (h *Handler) partitionReadyAttachments(ctx context.Context, tenantID uint64, sessionID string, ids []string) (ready []string, skipped int) {
	for _, id := range ids {
		doc, err := h.temporaryDocuments.Get(ctx, tenantID, sessionID, id)
		if err != nil || doc == nil || doc.Status != types.TemporaryDocumentStatusReady {
			skipped++
			continue
		}
		ready = append(ready, id)
	}
	return ready, skipped
}

// persistLastRequestState records the input-bar state the user just sent so
// that reopening this session restores agent/model/KB/web-search/MCP picks.
// Pure UI memo — failures are logged but never bubble up; the caller runs
// this in a goroutine and is safe to discard the returned context.
func (h *Handler) persistLastRequestState(parentCtx context.Context, reqCtx *qaRequestContext, mode qaMode) {
	// Detach from the HTTP request lifetime: this write must survive both
	// SSE disconnects and the parent gin context being released after the
	// handler returns.
	ctx := logger.CloneContext(context.WithoutCancel(parentCtx))

	agentEnabled := reqCtx.reqAgentEnabled
	// Mirror the resolution rule used in AgentQA: a resolved custom agent's
	// agent_mode wins over the request flag. For KnowledgeQA the request
	// itself carries agent_enabled=false, so this collapses correctly.
	if mode == qaModeAgent && reqCtx.customAgent != nil {
		agentEnabled = reqCtx.customAgent.IsAgentMode()
	}

	state := &types.SessionLastRequestState{
		AgentID:             reqCtx.reqAgentID,
		AgentEnabled:        agentEnabled,
		ModelID:             reqCtx.summaryModelID,
		ReasoningEffort:     reqCtx.reasoningEffort,
		KnowledgeBaseIDs:    reqCtx.knowledgeBaseIDs,
		KnowledgeIDs:        reqCtx.knowledgeIDs,
		TagIDs:              reqCtx.tagIDs,
		MCPServiceIDs:       reqCtx.mcpServiceIDs,
		SkillNames:          reqCtx.skillNames,
		MentionedItems:      reqCtx.mentionedItems,
		WebSearchEnabled:    reqCtx.webSearchEnabled,
		LocalBrowserEnabled: reqCtx.localBrowserEnabled,
	}

	if err := h.sessionService.UpdateSessionLastRequestState(ctx, reqCtx.sessionID, state); err != nil {
		logger.Warnf(ctx, "persist last_request_state failed for session %s: %v", reqCtx.sessionID, err)
	}
}

// appendQuickAnswerReasoning accumulates streamed reasoning_content from
// KnowledgeQA (fast answer) into a single AgentStep for history replay.
func appendQuickAnswerReasoning(msg *types.Message, content string) {
	if content == "" {
		return
	}
	ensureQuickAnswerStep(msg).ReasoningContent += content
}

// completeQuickAnswerTurn finishes a KnowledgeQA (fast-answer) turn.
// EventAgentComplete must run before the GORM write: handleComplete attaches
// SandboxCheckpoint (and artifacts) to the in-memory message, and
// completeAssistantMessage is the only persist on this path.
func (h *Handler) completeQuickAnswerTurn(
	ctx context.Context, streamCtx *sseStreamContext, query, userMessageID string,
) {
	if streamCtx == nil || streamCtx.assistantMessage == nil {
		return
	}
	// A stop can cancel the generation context after the final answer event
	// was queued. Preserve the streamed answer just as the Agent defer does.
	ctx = context.WithoutCancel(ctx)
	if streamCtx.eventBus != nil {
		// MessageID is what handleComplete keys on. Leave FinalAnswer empty:
		// KnowledgeQA already accumulated the answer on the message, and
		// handleComplete would append FinalAnswer a second time.
		_ = streamCtx.eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentComplete,
			SessionID: streamCtx.assistantMessage.SessionID,
			Data: event.AgentCompleteData{
				MessageID: streamCtx.assistantMessage.ID,
			},
		})
	}
	streamCtx.endTurn()
	h.completeStreamAssistantMessage(ctx, streamCtx, query, userMessageID, types.MessageStatusCompleted)
	if streamCtx.releaseTurn != nil {
		streamCtx.releaseTurn()
	}
}

// sessionTenantInfoContext makes the tenant info match the session owner (the
// context's tenant here). The chat history KB is read from the tenant info,
// and a shared agent's run carries the agent workspace's: the receiver's
// conversation would be indexed into the owner's chat history KB. When the
// session tenant cannot be loaded the message is not indexed at all.
func (h *Handler) sessionTenantInfoContext(ctx context.Context) (context.Context, bool) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	if info, ok := types.TenantInfoFromContext(ctx); ok && info != nil && info.ID == tenantID {
		return ctx, true
	}
	if tenantID == 0 || h.tenantService == nil {
		return ctx, false
	}
	tenant, err := h.tenantService.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		logger.Warnf(ctx, "Skipping chat history index: session tenant %d unavailable: %v", tenantID, err)
		return ctx, false
	}
	return context.WithValue(ctx, types.TenantInfoContextKey, tenant), true
}

// completeStreamAssistantMessage makes output readable before notifying clients
// to perform their final image/artifact fetch. A failed write must not announce
// a successful completion whose file authorization evidence is still missing.
// status is the terminal state the turn ends with when the write succeeds
// (completed, or stopped for a cancelled agent run); a turn that already ended
// as failed or interrupted keeps that state and is not announced as complete.
func (h *Handler) completeStreamAssistantMessage(
	ctx context.Context, streamCtx *sseStreamContext, query, userMessageID string, status types.MessageStatus,
) {
	streamCtx.endTurn()
	committed, err := h.persistStreamTurn(ctx, streamCtx, query, userMessageID, status)
	if err != nil {
		// The answer exists only in memory. End the turn as a persistence
		// failure before telling the client, so a reload right after the
		// error already shows it; if the database is still unavailable the
		// row stays generating and the reconciler ends it later (spec §6.1).
		h.finalizeTurn(ctx, streamCtx, turnOutcome{
			status: types.MessageStatusFailed,
			code:   types.TurnErrorCodePersistFailed,
			message: types.TurnFailedMessage(types.TurnErrorCodePersistFailed,
				types.LanguageFromContextOrDefault(ctx)),
		})
		if streamCtx.streamHandler != nil {
			_ = streamCtx.streamHandler.handleError(ctx, event.Event{
				ID: uuid.New().String(), Type: event.EventError, SessionID: streamCtx.assistantMessage.SessionID,
				Data: event.ErrorData{
					Stage:     "message_persistence",
					ErrorCode: types.TurnErrorCodePersistFailed,
					Error:     "Failed to save assistant message",
				},
			})
		}
		return
	}
	if committed != types.MessageStatusCompleted && committed != types.MessageStatusStopped {
		// failed / interrupted: a completion event would contradict the
		// stored state. The client must still learn that the turn is over:
		// a failure already sent its error frame, but a panic, a shutdown or
		// a row another path ended first (reconciler, another replica) did
		// not, and the client would otherwise wait on "replying".
		h.reportTurnEndedWithoutAnswer(ctx, streamCtx)
		return
	}
	if streamCtx.streamHandler != nil {
		if err := streamCtx.streamHandler.publishCompletion(ctx); err != nil {
			logger.Errorf(ctx, "Append persisted message completion failed: %v", err)
		}
	}
}

// turnPersistTimeout bounds the full persist of a turn. It carries content,
// steps and artifacts, so it gets more room than a status-only write.
var turnPersistTimeout = 30 * time.Second

// persistStreamTurn runs completeAssistantMessage for a streaming turn under
// its turn lock, with a deadline of its own.
func (h *Handler) persistStreamTurn(
	ctx context.Context, streamCtx *sseStreamContext, query, userMessageID string, status types.MessageStatus,
) (types.MessageStatus, error) {
	ctx, cancel := turnWriteContext(ctx, turnPersistTimeout)
	defer cancel()
	streamCtx.turnMu.Lock()
	defer streamCtx.turnMu.Unlock()
	return h.completeAssistantMessage(ctx, streamCtx.assistantMessage, query, userMessageID, status)
}

// completeAssistantMessage marks an assistant message as complete, updates it,
// and asynchronously indexes the Q&A pair into the chat history knowledge base.
//
// This is the full persist of a finished turn (content, steps, references,
// artifacts), so it is also how success and a user stop write their terminal
// state: status and is_completed go in the same row write (BF-03 C1). A turn
// that already decided its state in memory — failed or interrupted by
// finalizeTurn before this call — keeps that state instead of being relabelled,
// and a row another path already ended differently is not written at all: the
// in-memory message then takes the stored state. It returns the state the row
// is in afterwards. Only a completed or stopped turn runs the follow-up work
// (indexing, suggestions, memory); a failed or interrupted one has no answer.
func (h *Handler) completeAssistantMessage(
	ctx context.Context, assistantMessage *types.Message, userQuery, userMessageID string, status types.MessageStatus,
) (types.MessageStatus, error) {
	if assistantMessage == nil {
		return "", nil
	}
	prev := *assistantMessage
	if turnStatusDecided(assistantMessage) {
		status = assistantMessage.Status
	}
	assistantMessage.UpdatedAt = time.Now()
	assistantMessage.IsCompleted = true
	assistantMessage.Status = status
	applied, err := h.messageService.PersistTurnMessage(ctx, assistantMessage)
	if err != nil {
		// Nothing was stored: memory must not claim an ending the database
		// does not have, or the fallback finalize would skip the turn.
		assistantMessage.Status = prev.Status
		assistantMessage.IsCompleted = prev.IsCompleted
		assistantMessage.UpdatedAt = prev.UpdatedAt
		logger.Errorf(ctx, "Failed to persist assistant message %s: %v", assistantMessage.ID, err)
		return "", err
	}
	if !applied {
		return h.syncStoredTurnState(ctx, assistantMessage), nil
	}
	if status != types.MessageStatusCompleted && status != types.MessageStatusStopped {
		return status, nil
	}

	// Asynchronously index the Q&A pair into the chat history knowledge base for vector search.
	// Use WithoutCancel so the goroutine survives after the HTTP request context is done.
	bgCtx := context.WithoutCancel(ctx)
	if indexCtx, ok := h.sessionTenantInfoContext(bgCtx); ok {
		go h.messageService.IndexMessageToKB(
			indexCtx, userQuery, assistantMessage.Content, assistantMessage.ID, assistantMessage.SessionID)
	}
	if userQuery != "" && h.suggestionService != nil {
		go func() {
			if _, err := h.suggestionService.EnsureFollowUps(
				bgCtx, assistantMessage.SessionID, assistantMessage.ID, false,
			); err != nil {
				logger.Warnf(bgCtx, "follow-up suggestion generation failed for message %s: %v", assistantMessage.ID, err)
			}
		}()
	}
	if userQuery != "" {
		go h.recordTurnMemory(bgCtx, assistantMessage, userQuery, userMessageID)
	}
	return status, nil
}

// syncStoredTurnState makes the in-memory message agree with a row another
// path already ended, and returns that state. When the row cannot be read the
// turn is reported as interrupted: it is over, and not as this call wanted.
func (h *Handler) syncStoredTurnState(ctx context.Context, assistantMessage *types.Message) types.MessageStatus {
	stored, err := h.messageService.GetMessage(ctx, assistantMessage.SessionID, assistantMessage.ID)
	if err != nil || stored == nil {
		logger.Warnf(ctx, "Turn %s already ended elsewhere; stored state unreadable: %v", assistantMessage.ID, err)
		assistantMessage.Status = types.MessageStatusInterrupted
		return types.MessageStatusInterrupted
	}
	assistantMessage.Status = stored.EffectiveStatus()
	assistantMessage.ErrorCode, assistantMessage.ErrorMessage = stored.ErrorCode, stored.ErrorMessage
	assistantMessage.IsCompleted = stored.IsCompleted
	return assistantMessage.Status
}

// recordTurnMemory runs the long-term memory write path for a finished turn.
//
// This is the single place a conversation can produce memory, and it sits at
// the point where both the RAG and the Agent path converge, so neither mode
// can silently miss it. A stopped conversation arrives with an empty query and
// is skipped by the caller.
func (h *Handler) recordTurnMemory(
	ctx context.Context, assistantMessage *types.Message, userQuery, userMessageID string,
) {
	if h.memoryService == nil {
		return
	}
	// An explicit "remember ..." directive is stored verbatim and immediately,
	// with no model in the loop. This is what makes the default explicit_only
	// mode useful rather than merely safe.
	if statement, ok := types.DetectExplicitMemory(userQuery); ok {
		if _, err := h.memoryService.Remember(ctx, types.MemoryItem{
			Kind:            types.MemoryKindFact,
			Content:         statement,
			Importance:      4,
			Origin:          types.MemoryOriginExplicit,
			SourceSessionID: assistantMessage.SessionID,
			// Attribute to the user's own message, not the answer. Background
			// distillation reads that same message, so the two paths must
			// agree on provenance or a memory deleted from one can be
			// re-derived by the other.
			SourceMessageID: userMessageID,
		}); err != nil {
			logger.Warnf(ctx, "memory: explicit remember failed for message %s: %v", assistantMessage.ID, err)
		}
	}
	h.recordAnswerSources(ctx, assistantMessage)
	h.memoryService.ScheduleExtraction(ctx, assistantMessage.SessionID, assistantMessage.ID, assistantMessage.ModelID)
}

// recordAnswerSources notes which documents this answer drew on, so the
// reranker can prefer the material this person keeps working from.
//
// The references attached to an answer are a weaker signal than an explicit
// thumbs-up: they say the retriever kept picking a document, not that the user
// found it useful. They are, however, the only per-person retrieval signal
// available without asking for anything, and the boost they earn is capped
// accordingly.
func (h *Handler) recordAnswerSources(ctx context.Context, assistantMessage *types.Message) {
	if len(assistantMessage.KnowledgeReferences) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(assistantMessage.KnowledgeReferences))
	refs := make([]types.MemoryDocAffinity, 0, len(assistantMessage.KnowledgeReferences))
	for _, ref := range assistantMessage.KnowledgeReferences {
		if ref.KnowledgeID == "" {
			continue
		}
		if _, dup := seen[ref.KnowledgeID]; dup {
			continue
		}
		seen[ref.KnowledgeID] = struct{}{}
		refs = append(refs, types.MemoryDocAffinity{
			KnowledgeID:     ref.KnowledgeID,
			KnowledgeBaseID: ref.KnowledgeBaseID,
			Title:           ref.KnowledgeTitle,
		})
	}
	h.memoryService.RecordAnswerSources(ctx, refs)
}
