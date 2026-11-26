import type { AgentReadinessIssue, AgentSaveWarning } from '@/api/organization'
import type { AgentNotReadyReasonKey } from './agent-readiness'

/** Issue codes the backend reports (types.AgentReadinessIssue). */
export const AGENT_READINESS_ISSUE_CODES = [
  'missing_chat_model',
  'missing_rerank_model',
  'chat_model_unavailable',
  'rerank_model_unavailable',
] as const

export type AgentReadinessIssueCode = (typeof AGENT_READINESS_ISSUE_CODES)[number]

/** Warning code on a saved agent (types.AgentSaveWarning). */
export const SHARED_AGENT_NOT_READY = 'shared_agent_not_ready'

/**
 * i18n key per issue code. A code added on the backend later has no entry
 * here and is simply left out of the summary.
 */
const ISSUE_MESSAGE_KEYS: Record<AgentReadinessIssueCode, string> = {
  missing_chat_model: 'organization.share.issueMissingChatModel',
  missing_rerank_model: 'organization.share.issueMissingRerankModel',
  chat_model_unavailable: 'organization.share.issueChatModelUnavailable',
  rerank_model_unavailable: 'organization.share.issueRerankModelUnavailable',
}

/** Agent config path → the editor field that fixes it. */
const FIELD_HIGHLIGHTS: Record<string, AgentNotReadyReasonKey> = {
  'config.model_id': 'summary_model',
  'config.rerank_model_id': 'rerank_model',
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/**
 * The issue list as the server sends it. A malformed body yields an empty
 * list, so an old or unexpected answer falls back to the plain error message
 * instead of raising in the UI.
 */
export function parseAgentReadinessIssues(value: unknown): AgentReadinessIssue[] {
  if (!Array.isArray(value)) return []
  const issues: AgentReadinessIssue[] = []
  for (const item of value) {
    if (!isRecord(item) || typeof item.code !== 'string' || item.code === '') continue
    issues.push({ code: item.code, field: typeof item.field === 'string' ? item.field : '' })
  }
  return issues
}

/**
 * Issues from a refused share request. The server answers
 * `{ error: { code, message, details: { issues } } }`; the request helper
 * keeps that body on the result.
 */
export function shareRefusalIssues(response: unknown): AgentReadinessIssue[] {
  if (!isRecord(response) || !isRecord(response.error) || !isRecord(response.error.details)) return []
  return parseAgentReadinessIssues(response.error.details.issues)
}

/** Non-blocking warnings sent with a saved agent. */
export function parseAgentSaveWarnings(response: unknown): AgentSaveWarning[] {
  if (!isRecord(response) || !Array.isArray(response.warnings)) return []
  const warnings: AgentSaveWarning[] = []
  for (const item of response.warnings) {
    if (!isRecord(item) || typeof item.code !== 'string' || item.code === '') continue
    warnings.push({ code: item.code, issues: parseAgentReadinessIssues(item.issues) })
  }
  return warnings
}

/** The issues behind the "shared agent is no longer usable" save warning. */
export function sharedAgentNotReadyIssues(warnings: AgentSaveWarning[]): AgentReadinessIssue[] {
  for (const warning of warnings) {
    const issues = warning.issues ?? []
    if (warning.code === SHARED_AGENT_NOT_READY && issues.length > 0) {
      return issues
    }
  }
  return []
}

/** The editor field to highlight for one issue; null when it names none. */
export function agentReadinessHighlightField(issue: AgentReadinessIssue): AgentNotReadyReasonKey | null {
  return FIELD_HIGHLIGHTS[issue.field] ?? null
}

/** The first editor field worth highlighting for a list of issues. */
export function firstReadinessHighlightField(
  issues: readonly AgentReadinessIssue[],
): AgentNotReadyReasonKey | null {
  for (const issue of issues) {
    const field = agentReadinessHighlightField(issue)
    if (field) return field
  }
  return null
}

/** Whether anything in the list points at a field the editor can highlight. */
export function hasReadinessHighlightField(issues: readonly AgentReadinessIssue[]): boolean {
  return firstReadinessHighlightField(issues) !== null
}

/** Translated labels of the issues, without repeats and unknown codes. */
export function agentReadinessIssueLabels(
  t: (key: string) => string,
  issues: readonly AgentReadinessIssue[],
): string[] {
  const labels: string[] = []
  for (const issue of issues) {
    const key = (ISSUE_MESSAGE_KEYS as Record<string, string>)[issue.code]
    if (!key) continue
    const label = t(key)
    if (label && !labels.includes(label)) labels.push(label)
  }
  return labels
}

/**
 * Whether a shared agent's saved configuration is incomplete. The verdict is
 * the server's: a receiver cannot check another workspace's models. An absent
 * `ready` (older backend) is not a verdict and falls back to local checks.
 */
export function isSharedAgentNotReady(info: { ready?: boolean } | null | undefined): boolean {
  return info?.ready === false
}
