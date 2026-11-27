package session

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/primind/primind/internal/errors"
	"github.com/primind/primind/internal/event"
	"github.com/primind/primind/internal/logger"
	"github.com/primind/primind/internal/storageurl"
	"github.com/primind/primind/internal/types"
	"github.com/primind/primind/internal/types/interfaces"
	secutils "github.com/primind/primind/internal/utils"
	"gorm.io/gorm"
)

// ContinueStream godoc
// @Summary      继续流式响应
// @Description  继续获取正在进行的流式响应
// @Description  消息已是终态（completed / failed / stopped / interrupted）时返回 404，响应体带 status 字段；
// @Description  消息仍为 generating、流事件已过期、且没有进行中的轮次时，把消息写为 interrupted（error_code=process_lost），
// @Description  并以 SSE 返回一个 response_type=error、done=true、data.code=turn_interrupted 的事件后结束连接
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id     path      string  true   "会话ID"
// @Param        message_id     query     string  true   "消息ID"
// @Param        resource_urls  query     string  false  "文件引用形式，public 返回可加载直链"  Enums(handle, public)  default(handle)
// @Success      200            {object}  map[string]interface{}  "流式响应"
// @Failure      404         {object}  map[string]interface{}  "会话或消息不存在，或消息已不再生成（响应带 status）"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/continue-stream/{session_id} [get]
func (h *Handler) ContinueStream(c *gin.Context) {
	ctx := c.Request.Context()

	logger.Info(ctx, "Start continuing stream response processing")

	// Get session ID from URL parameter
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		c.Error(errors.NewBadRequestError(errors.ErrInvalidSessionID.Error()))
		return
	}

	// Get message ID from query parameter
	messageID := secutils.SanitizeForLog(c.Query("message_id"))
	if messageID == "" {
		logger.Error(ctx, "Message ID is empty")
		c.Error(errors.NewBadRequestError("Missing message ID"))
		return
	}

	logger.Infof(ctx, "Continuing stream, session ID: %s, message ID: %s", sessionID, messageID)

	// Resolve before any SSE header is written so an invalid resource_urls value
	// is still reportable as a normal 400 JSON error.
	resourceRewriter, err := h.resolveStreamRewriter(c)
	if err != nil {
		logger.Warnf(ctx, "Rejected resource URL mode: %v", err)
		_ = c.Error(err)
		return
	}

	// Verify that the session exists and belongs to this tenant
	if _, err := h.sessionService.GetSession(ctx, sessionID); err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			logger.Warnf(ctx, "Session not found, ID: %s", sessionID)
			c.Error(errors.NewNotFoundError(err.Error()))
		} else {
			logger.ErrorWithFields(ctx, err, nil)
			c.Error(errors.NewInternalServerError(err.Error()))
		}
		return
	}

	// Get the incomplete message
	message, err := h.messageService.GetMessage(ctx, sessionID, messageID)
	if err != nil {
		if stderrors.Is(err, errors.ErrSessionNotFound) {
			// PR #1309 plumbed user-scope into messageService.GetMessage's
			// session existence check; non-owner / wrong-user lookups now
			// surface as ErrSessionNotFound. Map to 404 so clients can tell
			// "wrong URL" from a real 5xx instead of seeing a generic 500.
			logger.Warnf(ctx, "Session not found, ID: %s", sessionID)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			// The message_id doesn't exist (e.g. a wrong / non-persisted id, or an
			// expired replay buffer). That is a client error, not a server fault:
			// return 404 so callers read resource.not_found (a permanent condition
			// they must not retry) instead of a retryable 5xx. Mirrors the
			// ErrSessionNotFound branch above and the kb/doc/chunk not-found fix.
			logger.Warnf(ctx, "Message not found, session ID: %s, message ID: %s", sessionID, messageID)
			c.Error(errors.NewNotFoundError(err.Error()))
			return
		}
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	// A message that is no longer generating has nothing to resume. The 404
	// carries which terminal state it is in so the client renders it as is
	// instead of guessing (BF-03 §4.3 case 1).
	if message == nil || message.EffectiveStatus() != types.MessageStatusGenerating {
		status := ""
		if message != nil {
			status = string(message.EffectiveStatus())
		}
		logger.Warnf(ctx, "Incomplete message not found, session ID: %s, message ID: %s, status: %s",
			sessionID, messageID, status)
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "Incomplete message not found",
			"status":  status,
		})
		return
	}

	// Get initial events from stream (offset 0)
	events, currentOffset, err := h.streamManager.GetEvents(ctx, sessionID, messageID, 0)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(fmt.Sprintf("Failed to get stream data: %s", err.Error())))
		return
	}

	if len(events) == 0 {
		// The stream is gone (it expires an hour after the last event) and
		// the message never finished. If nothing is driving the turn either,
		// end it here instead of failing every future open (BF-03 §4.3 case 3).
		if h.healAbandonedTurn(ctx, c, sessionID, message) {
			return
		}
		logger.Warnf(ctx, "No events found in stream, session ID: %s, message ID: %s", sessionID, messageID)
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"error":   "No stream events found",
			"status":  string(types.MessageStatusGenerating),
		})
		return
	}

	logger.Infof(
		ctx, "Preparing to replay %d events and continue streaming, session ID: %s, message ID: %s",
		len(events), sessionID, messageID,
	)

	// Set headers for SSE
	setSSEHeaders(c)

	// Check if stream is already completed
	streamCompleted := false
	for _, evt := range events {
		if evt.Type == "complete" {
			streamCompleted = true
			break
		}
	}

	// Replay existing events, a segment's worth of chunks per frame
	replay := coalesceReplayEvents(events)
	logger.Debugf(ctx, "Replaying %d existing events as %d frames", len(events), len(replay))
	for _, evt := range replay {
		emitStreamEvent(ctx, c, evt, message.RequestID, resourceRewriter)
	}

	// If stream is already completed, send final event and return
	if streamCompleted {
		logger.Infof(ctx, "Stream already completed, session ID: %s, message ID: %s", sessionID, messageID)
		sendCompletionEvent(c, message.RequestID)
		return
	}

	// Continue polling for new events
	logger.Debug(ctx, "Starting event update monitoring")
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			logger.Debug(ctx, "Client connection closed")
			return

		case <-ticker.C:
			// Get new events from current offset
			newEvents, newOffset, err := h.streamManager.GetEvents(ctx, sessionID, messageID, currentOffset)
			if err != nil {
				logger.Errorf(ctx, "Failed to get new events: %v", err)
				flushHeldStreamContent(ctx, c, message.RequestID, resourceRewriter)
				return
			}

			// Send new events
			streamCompletedNow := false
			for _, evt := range newEvents {
				// Check for completion event
				if evt.Type == "complete" {
					streamCompletedNow = true
				}

				emitStreamEvent(ctx, c, evt, message.RequestID, resourceRewriter)
			}

			// Update offset
			currentOffset = newOffset

			// If stream completed, send final event and exit
			if streamCompletedNow {
				logger.Infof(ctx, "Stream completed, session ID: %s, message ID: %s", sessionID, messageID)
				sendCompletionEvent(c, message.RequestID)
				return
			}
		}
	}
}

// turnInterruptedStage names the error event healAbandonedTurn emits.
const turnInterruptedStage = "turn_interrupted"

// healAbandonedTurn ends a turn whose message is still marked generating but
// whose stream has expired and which nothing is driving any more, then
// answers the client with a single error event (BF-03 §4.3 case 3).
//
// Four checks keep a live turn safe:
//   - this process still generating the turn (quick answers publish no
//     marker, so the in-process registry is their only witness here);
//   - the live-run marker still naming this message means an engine is
//     generating it and simply has not emitted anything yet. Only the
//     generating process extends the marker (startLiveRunHeartbeat), so a
//     killed engine's marker expires instead of being kept alive by the very
//     reads that ask about it;
//   - a message younger than the grace period may be between its persist and
//     its first event or marker (R1);
//   - an unreadable marker is not proof that nothing runs.
//
// It returns true when it took over the response. The write is the
// conditional FinalizeMessage, so a turn that ends concurrently is untouched.
func (h *Handler) healAbandonedTurn(
	ctx context.Context, c *gin.Context, sessionID string, message *types.Message,
) bool {
	if h.turns.IsLive(sessionID, message.ID) {
		logger.Infof(ctx, "continue-stream: turn still generating in this process, session ID: %s, message ID: %s",
			sessionID, message.ID)
		return false
	}
	liveID, _, err := h.streamManager.GetLiveRun(ctx, sessionID)
	if err != nil {
		logger.Warnf(ctx, "continue-stream: live-run lookup failed for session %s: %v", sessionID, err)
		return false
	}
	if liveID != "" && liveID == message.ID {
		logger.Infof(ctx, "continue-stream: turn still live without events yet, session ID: %s, message ID: %s",
			sessionID, message.ID)
		return false
	}
	if time.Since(message.UpdatedAt) < types.ContinueStreamGracePeriod {
		return false
	}

	if _, err := h.messageService.FinalizeMessage(ctx, sessionID, message.ID,
		types.MessageStatusInterrupted, types.TurnErrorCodeProcessLost,
		types.TurnInterruptedMessage(types.LanguageFromContextOrDefault(ctx)), nil); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": message.ID,
		})
		return false
	}
	logger.Warnf(ctx, "continue-stream: interrupted abandoned turn, session ID: %s, message ID: %s, idle_for: %s",
		sessionID, message.ID, time.Since(message.UpdatedAt).Round(time.Second))

	setSSEHeaders(c)
	c.SSEvent("message", &types.StreamResponse{
		ID:           message.RequestID,
		ResponseType: types.ResponseTypeError,
		Content:      types.TurnInterruptedMessage(types.LanguageFromContextOrDefault(ctx)),
		Done:         true,
		SessionID:    sessionID,
		Data: map[string]interface{}{
			"stage":      turnInterruptedStage,
			"code":       types.TurnSSECodeInterrupted,
			"error_code": types.TurnErrorCodeProcessLost,
			"status":     string(types.MessageStatusInterrupted),
		},
	})
	c.Writer.Flush()
	return true
}

// StopSession godoc
// @Summary      停止生成
// @Description  停止当前正在进行的生成任务
// @Tags         问答
// @Accept       json
// @Produce      json
// @Param        session_id  path      string              true  "会话ID"
// @Param        request     body      StopSessionRequest  true  "停止请求"
// @Success      200         {object}  map[string]interface{}  "停止成功"
// @Failure      404         {object}  errors.AppError         "会话或消息不存在"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/stop [post]
func (h *Handler) StopSession(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))

	if sessionID == "" {
		c.JSON(400, gin.H{"error": "Session ID is required"})
		return
	}

	// Parse request body to get message_id
	var req StopSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
		})
		c.JSON(400, gin.H{"error": "message_id is required"})
		return
	}

	assistantMessageID := secutils.SanitizeForLog(req.MessageID)
	logger.Infof(ctx, "Stop generation request for session: %s, message: %s", sessionID, assistantMessageID)

	// Get tenant ID from context
	tenantID, exists := c.Get(types.TenantIDContextKey.String())
	if !exists {
		logger.Error(ctx, "Failed to get tenant ID")
		c.JSON(401, gin.H{"error": "Unauthorized"})
		return
	}
	tenantIDUint := tenantID.(uint64)

	// Verify message ownership and status
	message, err := h.messageService.GetMessage(ctx, sessionID, assistantMessageID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
		})
		c.JSON(404, gin.H{"error": "Message not found"})
		return
	}

	// Verify message belongs to this session (double check)
	if message.SessionID != sessionID {
		logger.Warnf(ctx, "Message %s does not belong to session %s", assistantMessageID, sessionID)
		c.JSON(403, gin.H{"error": "Message does not belong to this session"})
		return
	}

	// Verify message belongs to the current tenant. Stopping generation mutates
	// an in-flight run, so use the strict owner scope: a tenant admin may read an
	// API-key session but must not be able to interrupt its (external) API calls.
	session, err := h.sessionService.GetOwnedSession(ctx, sessionID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
		})
		c.JSON(404, gin.H{"error": "Session not found"})
		return
	}

	if session.TenantID != tenantIDUint {
		logger.Warnf(ctx, "Session %s does not belong to tenant %d", sessionID, tenantIDUint)
		c.JSON(403, gin.H{"error": "Access denied"})
		return
	}

	// Check if message is already completed (stopped)
	if message.IsCompleted {
		logger.Infof(ctx, "Message %s is already completed, no need to stop", assistantMessageID)
		c.JSON(200, gin.H{
			"success": true,
			"message": "Message already completed",
		})
		return
	}

	// Write stop event to StreamManager for distributed support
	stopEvent := interfaces.StreamEvent{
		ID:        fmt.Sprintf("stop-%d", time.Now().UnixNano()),
		Type:      types.ResponseType(event.EventStop),
		Content:   "",
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
			"reason":     "user_requested",
		},
	}

	if err := h.streamManager.AppendEvent(ctx, sessionID, assistantMessageID, stopEvent); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"session_id": sessionID,
			"message_id": assistantMessageID,
		})
		c.JSON(500, gin.H{"error": "Failed to write stop event"})
		return
	}

	logger.Infof(ctx, "Stop event written successfully for session: %s, message: %s", sessionID, assistantMessageID)
	c.JSON(200, gin.H{
		"success": true,
		"message": "Generation stopped",
	})
}

// handleAgentEventsForSSE handles agent events for SSE streaming using an existing handler
// The handler is already subscribed to events and AgentQA is already running
// This function polls StreamManager and pushes events to SSE, allowing graceful handling of disconnections
// waitForTitle: if true, wait for title event after completion (for new sessions without title)
func (h *Handler) handleAgentEventsForSSE(
	ctx context.Context,
	c *gin.Context,
	sessionID, assistantMessageID, requestID string,
	eventBus *event.EventBus,
	waitForTitle bool,
	resourceRewriter *storageurl.StreamRewriter,
) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	lastOffset := 0
	log := logger.GetLogger(ctx)

	log.Infof("Starting pull-based SSE streaming for session=%s, message=%s", sessionID, assistantMessageID)

	for {
		select {
		case <-c.Request.Context().Done():
			// Connection closed, exit gracefully without panic
			log.Infof(
				"Client disconnected, stopping SSE streaming for session=%s, message=%s",
				sessionID,
				assistantMessageID,
			)
			return

		case <-ticker.C:
			// Get new events from StreamManager using offset
			events, newOffset, err := h.streamManager.GetEvents(ctx, sessionID, assistantMessageID, lastOffset)
			if err != nil {
				log.Warnf("Failed to get events from stream: %v", err)
				continue
			}

			// Send any new events
			streamCompleted := false
			titleReceived := false
			for _, evt := range events {
				// Check for stop event
				if evt.Type == types.ResponseType(event.EventStop) {
					log.Infof("Detected stop event, triggering stop via EventBus for session=%s", sessionID)

					// Emit stop event to the EventBus to trigger context cancellation
					if eventBus != nil {
						eventBus.Emit(ctx, event.Event{
							Type:      event.EventStop,
							SessionID: sessionID,
							Data: event.StopData{
								SessionID: sessionID,
								MessageID: assistantMessageID,
								Reason:    "user_requested",
							},
						})
					}

					// Release any buffered tail first: the answer generated
					// before the stop is still the user's content, and in
					// public resource URL mode part of it may be sitting in
					// the holdback buffer.
					flushHeldStreamContent(ctx, c, requestID, resourceRewriter)

					// Send stop notification to frontend
					c.SSEvent("message", &types.StreamResponse{
						ID:           requestID,
						ResponseType: "stop",
						Content:      "Generation stopped by user",
						Done:         true,
					})
					c.Writer.Flush()
					return
				}

				// Check for completion event
				if evt.Type == "complete" {
					streamCompleted = true
				}

				// Check for title event
				if evt.Type == types.ResponseTypeSessionTitle {
					titleReceived = true
				}

				// Check if connection is still alive before writing. Build the
				// payload only after this check: in public resource URL mode
				// building consumes the chunk into the holdback buffer, so an
				// early return here would drop it.
				if c.Request.Context().Err() != nil {
					log.Info("Connection closed during event sending, stopping")
					return
				}

				emitStreamEvent(ctx, c, evt, requestID, resourceRewriter)
			}

			// Update offset
			lastOffset = newOffset

			// Check if stream is completed - wait for title event only if needed and not already received
			if streamCompleted {
				if waitForTitle && !titleReceived {
					log.Infof("Stream completed for session=%s, message=%s, waiting for title event", sessionID, assistantMessageID)
					// Wait up to 3 seconds for title event after completion
					titleTimeout := time.After(3 * time.Second)
				titleWaitLoop:
					for {
						select {
						case <-titleTimeout:
							log.Info("Title wait timeout, closing stream")
							break titleWaitLoop
						case <-c.Request.Context().Done():
							log.Info("Connection closed while waiting for title")
							return
						default:
							// Check for new events (title event)
							events, newOff, err := h.streamManager.GetEvents(c.Request.Context(), sessionID, assistantMessageID, lastOffset)
							if err != nil {
								log.Warnf("Error getting events while waiting for title: %v", err)
								break titleWaitLoop
							}
							if len(events) > 0 {
								for _, evt := range events {
									emitStreamEvent(ctx, c, evt, requestID, resourceRewriter)
									// If we got the title, we can exit
									if evt.Type == types.ResponseTypeSessionTitle {
										log.Infof("Title event received: %s", evt.Content)
										break titleWaitLoop
									}
								}
								lastOffset = newOff
							} else {
								// No events, wait a bit before checking again
								time.Sleep(100 * time.Millisecond)
							}
						}
					}
				} else {
					log.Infof("Stream completed for session=%s, message=%s", sessionID, assistantMessageID)
				}
				sendCompletionEvent(c, requestID)
				return
			}
		}
	}
}
