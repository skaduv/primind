/**
 * Terminal state of an assistant turn (BF-03).
 *
 * The backend stores `status` on every assistant message and infers it for
 * rows that predate the column from `is_completed`. The same rule lives here so
 * old backends (no `status` field at all) keep working with new front code:
 * only a message that is really still generating resumes its stream.
 */
export type MessageTurnStatus = 'generating' | 'completed' | 'failed' | 'stopped' | 'interrupted'

/** Shape of the message fields these helpers read. */
export interface TurnStatusMessage {
  role?: unknown
  status?: unknown
  is_completed?: unknown
  error_code?: unknown
  error_message?: unknown
}

/** `data.code` of the single error event continue-stream returns after it repaired an abandoned turn. */
export const TURN_INTERRUPTED_SSE_CODE = 'turn_interrupted'

/** `error_code` the backend records for a turn nobody was driving any more. */
export const TURN_ERROR_PROCESS_LOST = 'process_lost'

/** Same rule as the backend's Message.EffectiveStatus: an explicit status wins, else is_completed decides. */
export function effectiveTurnStatus(message: TurnStatusMessage | null | undefined): MessageTurnStatus {
  if (!message) return 'completed'
  if (typeof message.status === 'string' && message.status) {
    return message.status as MessageTurnStatus
  }
  return message.is_completed ? 'completed' : 'generating'
}

/** True only while the backend still considers the assistant turn generating. */
export function isGeneratingTurn(message: TurnStatusMessage | null | undefined): boolean {
  return message?.role === 'assistant' && effectiveTurnStatus(message) === 'generating'
}

/** A turn that ended without an answer the user can use: show why, and offer to ask again. */
export function isFailedOrInterruptedTurn(message: TurnStatusMessage | null | undefined): boolean {
  if (message?.role !== 'assistant') return false
  const status = effectiveTurnStatus(message)
  return status === 'failed' || status === 'interrupted'
}

/** Recognizes the quiet "this turn was interrupted" event of continue-stream. */
export function isTurnInterruptedEvent(
  data: { response_type?: unknown; data?: unknown; [key: string]: unknown } | null | undefined,
): boolean {
  return data?.response_type === 'error'
    && (data.data as { code?: unknown } | undefined)?.code === TURN_INTERRUPTED_SSE_CODE
}

/**
 * What a continue-stream 404 means for the turn we tried to resume, from the
 * `status` the backend puts in the 404 body (BF-03):
 *  - `retry`: still `generating` on the server — the run has not emitted its
 *    first event yet, or the backend's grace period has not passed. Try again
 *    shortly; once the grace period is over the backend either streams or ends
 *    the turn with a `turn_interrupted` event.
 *  - `reload`: the turn already ended on the server (completed, stopped,
 *    failed, interrupted) between the history load and the resume. Reload the
 *    history so the bubble shows what was actually saved, instead of guessing.
 *  - `interrupted`: no status (an older backend). Nothing is known beyond
 *    "there is no stream", so show the turn as interrupted.
 */
export type ResumeNotFoundAction = 'retry' | 'reload' | 'interrupted'

export function resumeNotFoundAction(status: string | null | undefined): ResumeNotFoundAction {
  switch (status) {
    case 'generating':
      return 'retry'
    case 'completed':
    case 'stopped':
    case 'failed':
    case 'interrupted':
      return 'reload'
    default:
      return 'interrupted'
  }
}

/** Delay between resume attempts while the backend still reports `generating`. */
export const RESUME_RETRY_INTERVAL_MS = 5000

/**
 * Attempts before giving up on a turn the backend keeps reporting as
 * `generating` without a stream: 30 × 5s outlasts the backend's 2-minute
 * grace period, after which it heals an abandoned turn itself.
 */
export const RESUME_RETRY_MAX_ATTEMPTS = 30

/** The machine-readable reason stored on a failed or interrupted message. */
export function turnErrorCode(message: TurnStatusMessage | null | undefined): string {
  return typeof message?.error_code === 'string' ? message.error_code : ''
}

/** The explanation the backend stored for the user, if any. */
export function turnErrorMessage(message: TurnStatusMessage | null | undefined): string {
  return typeof message?.error_message === 'string' ? message.error_message : ''
}

/**
 * Fill a message with the state continue-stream just reported for it. Used for
 * the interrupted event and for a resume that found nothing to resume.
 */
export function applyTurnOutcome(
  message: Record<string, unknown>,
  outcome: { status: MessageTurnStatus; error_code?: string; error_message?: string },
): void {
  message.is_completed = true
  message.status = outcome.status
  message.error_code = outcome.error_code || ''
  message.error_message = outcome.error_message || ''
  message.thinking = false
  message.artifactsCollecting = false
}
