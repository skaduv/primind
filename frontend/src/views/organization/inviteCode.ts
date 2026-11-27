/**
 * Invite-code helpers for the "join shared space" flow (BF-06).
 *
 * Pure functions only: no Vue, no i18n instance. The i18n keys are returned
 * as strings so the page can call `t()` and these rules stay unit-testable.
 */

/** Business error codes the backend returns for invite-code lookups (internal/errors, 2400-2499). */
export const INVITE_CODE_NOT_FOUND = 2400
export const INVITE_CODE_EXPIRED = 2401

const INVITE_ERROR_KEYS: Readonly<Record<number, string>> = {
  [INVITE_CODE_EXPIRED]: 'organization.invite.codeExpired',
  [INVITE_CODE_NOT_FOUND]: 'organization.invite.codeNotFound',
}

/** Keys for failures the backend identifies by HTTP status alone. */
const INVITE_STATUS_KEYS = {
  /** A 404 without a typed code: a backend older than the typed codes, which could not tell the two apart. */
  legacyNotFound: 'organization.invite.invalidCode',
  /**
   * 5xx or no response at all: the service, not the invite, is the problem.
   * Default only; callers pass the "try again" key of the action that failed
   * (join and join request must not read 「预览失败」).
   */
  unavailable: 'organization.invite.previewFailed',
} as const

/** What a failed request looks like after the API wrappers normalized it. */
export interface InviteFailure {
  code?: number
  status?: number
  message?: string
}

/**
 * The i18n key for a failure that is about the invite code or the service
 * behind it, or null for anything else.
 *
 * The backend's own message is never returned: it is English, and for the
 * known cases it is also less specific than the translation.
 *
 * `unavailableKey` is returned for 5xx and network errors, so each action
 * says what failed: preview, join or join request.
 */
export function knownInviteErrorKey(
  result: InviteFailure,
  unavailableKey: string = INVITE_STATUS_KEYS.unavailable,
): string | null {
  if (result.code !== undefined) {
    const typed = INVITE_ERROR_KEYS[result.code]
    if (typed) return typed
  }
  const { status } = result
  // Checked after the typed codes: the old backend answered 404 with the
  // generic not-found code, the new one with 2400 / 2401.
  if (status === 404) return INVITE_STATUS_KEYS.legacyNotFound
  if (status === undefined || status >= 500) return unavailableKey
  return null
}

/**
 * Maps a failed invite-code request to the i18n key the page should show.
 *
 * `fallbackKey` is the action's own "failed, try again" key. It is used for
 * 5xx and network errors, and for everything that is not an invite-code
 * problem (space full, request already pending, ...): those keep the backend
 * message, see inviteFailureMessage.
 */
export function inviteErrorKey(result: InviteFailure, fallbackKey: string): string {
  return knownInviteErrorKey(result, fallbackKey) ?? fallbackKey
}

/**
 * The text to show for a failed invite-code request. Known invite-code and
 * service failures are translated and never show the English original.
 * Everything else keeps the backend's own message ("space full", "request
 * already pending"...); when there is none, the translated fallback is used.
 *
 * `t` is passed in so this module stays free of the i18n instance.
 */
export function inviteFailureMessage(result: InviteFailure, fallbackKey: string, t: (key: string) => string): string {
  // 5xx and network errors: the action's own translated "try again" text.
  const known = knownInviteErrorKey(result, fallbackKey)
  if (known) return t(known)
  return result.message || t(fallbackKey)
}

const INVITE_CODE_QUERY_KEYS = ['code', 'invite_code'] as const

/**
 * Extracts the invite code from whatever the member pasted into the input:
 * a bare code, or a whole invite link. Links are recognized by the query
 * string they carry — `/join?code=<code>` is what the settings page copies
 * and `/platform/organizations?invite_code=<code>` is where it lands —
 * whether absolute or relative. Anything else comes back trimmed, so the
 * caller only has to handle one string.
 */
export function extractInviteCode(input: string): string {
  const trimmed = input.trim()
  if (!trimmed || !trimmed.includes('?')) return trimmed

  let url: URL
  try {
    // The base is a placeholder: only the query string is read, so the
    // origin of a relative link does not matter.
    url = new URL(trimmed, 'http://invite.local')
  } catch {
    return trimmed
  }
  for (const key of INVITE_CODE_QUERY_KEYS) {
    const value = url.searchParams.get(key)
    if (value) return value.trim()
  }
  return trimmed
}
