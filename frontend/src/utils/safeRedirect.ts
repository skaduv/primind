/**
 * Post-login return address handling (BF-06).
 *
 * A user who opens an invite link while logged out is sent to /login with
 * `?redirect=<original address>`; after signing in they should land back on
 * that address instead of the default home. The value comes from the URL, so
 * it is untrusted input: only a same-site path may be followed, otherwise the
 * parameter turns the login page into an open redirect.
 */

/** Where to return after a round trip that leaves the SPA (OIDC). sessionStorage: per tab, gone when the tab closes. */
export const PENDING_LOGIN_REDIRECT_KEY = 'primind_login_redirect'

/** Pages that must never be a return target: following one would bounce straight back to the login page. */
const LOGIN_PAGES = new Set(['/login', '/register'])

/**
 * Returns `value` when it is a same-site path the login page may send the
 * user to, otherwise null.
 *
 * Only a string starting with a single "/" is accepted. Everything a browser
 * would resolve to another origin is refused: protocol-relative "//host" and
 * "/\", which browsers normalize to "//" and treat as a scheme-relative URL.
 * Absolute URLs ("https://…") do not start with "/" and are refused for that
 * reason. The login and register pages are refused too so a stale `redirect`
 * cannot loop.
 */
export function safeRedirect(value: unknown): string | null {
  if (typeof value !== 'string') return null
  if (!value.startsWith('/')) return null
  // Second character decides between a path and another origin. Control
  // characters are included: browsers strip them before parsing, so
  // "/\t/evil.com" would come out as "//evil.com".
  const second = value.charAt(1)
  if (second === '/' || second === '\\' || second === '\t' || second === '\n' || second === '\r') return null
  // Also refuse a backslash anywhere in the path: some client-side routers
  // normalize it to a slash before matching, which could re-create "//".
  const path = value.split(/[?#]/, 1)[0]
  if (path.includes('\\')) return null

  const normalized = path.toLowerCase().replace(/\/+$/, '') || '/'
  if (LOGIN_PAGES.has(normalized)) return null
  return value
}

/**
 * Remembers a return address for a login that leaves the SPA (OIDC). Unsafe
 * or missing values clear any previous one, so nothing stale survives.
 */
export function stashPendingLoginRedirect(value: unknown, storage: Pick<Storage, 'setItem' | 'removeItem'>): void {
  const target = safeRedirect(value)
  if (target) storage.setItem(PENDING_LOGIN_REDIRECT_KEY, target)
  else storage.removeItem(PENDING_LOGIN_REDIRECT_KEY)
}

/** Reads and clears the stashed return address; null when there is none or it is not safe. */
export function consumePendingLoginRedirect(storage: Pick<Storage, 'getItem' | 'removeItem'>): string | null {
  const stashed = storage.getItem(PENDING_LOGIN_REDIRECT_KEY)
  storage.removeItem(PENDING_LOGIN_REDIRECT_KEY)
  return safeRedirect(stashed)
}

/**
 * Where the login page sends a user after a successful login. The return
 * address is honored only for a user who already has a workspace: without
 * one the onboarding page comes first and is not skipped.
 */
export function postLoginTarget(redirect: string | null, hasValidTenant: boolean): string {
  if (hasValidTenant && redirect) return redirect
  return hasValidTenant ? '/platform/knowledge-bases' : '/onboarding/workspace'
}
