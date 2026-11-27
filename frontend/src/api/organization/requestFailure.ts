import type { ApiResponse } from './index'

/**
 * Anything the request layer may reject with. The interceptor spreads the
 * response body onto the thrown object and records the HTTP status, so a
 * backend failure arrives as `{ status, message, error: { code, message } }`;
 * a network error arrives as a bare `{ message }`.
 */
export interface RequestRejection {
  message?: string
  status?: number
  /** Non-enumerable HTTP status the interceptor attaches to JSON payloads. */
  $httpStatus?: number
  error?: { code?: unknown } | string
}

/** The failure half of {@link ApiResponse}: what callers get when a request did not succeed. */
export type ApiFailure = ApiResponse<never>

/**
 * Normalizes a rejected request into the failure shape callers already
 * consume, keeping the two things a page needs to tell failures apart:
 * the business `code` (read from `error.code`, the same place modelUsage
 * takes it) and the HTTP `status`. A network error carries neither.
 *
 * `code` and `status` are omitted rather than set to undefined so the
 * object stays easy to compare and to serialize in tests.
 */
export function requestFailure(e: RequestRejection | undefined | null, fallbackMessage: string): ApiFailure {
  const failure: ApiFailure = { success: false, message: e?.message || fallbackMessage }

  const code = typeof e?.error === 'object' && e.error !== null ? e.error.code : undefined
  if (typeof code === 'number') failure.code = code

  const status = typeof e?.status === 'number' ? e.status : e?.$httpStatus
  if (typeof status === 'number') failure.status = status

  return failure
}
