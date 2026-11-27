import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  PENDING_LOGIN_REDIRECT_KEY,
  consumePendingLoginRedirect,
  postLoginTarget,
  safeRedirect,
  stashPendingLoginRedirect,
} from './safeRedirect.ts'

function memoryStorage(initial: Record<string, string> = {}): Storage {
  const data = new Map(Object.entries(initial))
  return {
    get length() {
      return data.size
    },
    clear: () => data.clear(),
    getItem: (key) => data.get(key) ?? null,
    key: (index) => [...data.keys()][index] ?? null,
    removeItem: (key) => void data.delete(key),
    setItem: (key, value) => void data.set(key, value),
  }
}

test('safeRedirect accepts a same-site path with a query string', () => {
  assert.equal(safeRedirect('/platform/organizations?invite_code=abc'), '/platform/organizations?invite_code=abc')
  assert.equal(safeRedirect('/join?code=abc#top'), '/join?code=abc#top')
  assert.equal(safeRedirect('/'), '/')
})

test('safeRedirect refuses everything that could leave the site', () => {
  for (const value of [
    '//evil.com',
    '//evil.com/path',
    '///evil.com',
    '/\\evil.com',
    '/\\\\evil.com',
    '/a/\\evil.com',
    '/\tevil.com',
    '/\n/evil.com',
    'https://evil.com',
    'http://evil.com/join?code=abc',
    'javascript:alert(1)',
    'evil.com/join',
    ' /platform/organizations',
  ]) {
    assert.equal(safeRedirect(value), null, JSON.stringify(value))
  }
})

test('safeRedirect refuses the login pages so a return address cannot loop', () => {
  for (const value of ['/login', '/login/', '/LOGIN', '/login?redirect=/login', '/register?token=abc', '/Register/']) {
    assert.equal(safeRedirect(value), null, JSON.stringify(value))
  }
  // Other pages keep working even when they look similar.
  assert.equal(safeRedirect('/login-help'), '/login-help')
})

test('safeRedirect refuses non-string and empty input', () => {
  for (const value of [undefined, null, 123, {}, ['/x'], '']) {
    assert.equal(safeRedirect(value), null, JSON.stringify(value) ?? String(value))
  }
})

test('postLoginTarget honors the return address only with a workspace', () => {
  assert.equal(postLoginTarget('/platform/organizations?invite_code=abc', true), '/platform/organizations?invite_code=abc')
  // No workspace yet: onboarding comes first and the join window is not restored (documented non-goal).
  assert.equal(postLoginTarget('/platform/organizations?invite_code=abc', false), '/onboarding/workspace')
  assert.equal(postLoginTarget(null, true), '/platform/knowledge-bases')
  assert.equal(postLoginTarget(null, false), '/onboarding/workspace')
})

test('the OIDC return address is stashed, consumed once and never trusted blindly', () => {
  const storage = memoryStorage()
  stashPendingLoginRedirect('/platform/organizations?invite_code=abc', storage)
  assert.equal(storage.getItem(PENDING_LOGIN_REDIRECT_KEY), '/platform/organizations?invite_code=abc')
  assert.equal(consumePendingLoginRedirect(storage), '/platform/organizations?invite_code=abc')
  assert.equal(consumePendingLoginRedirect(storage), null, 'consumed on first read')

  // An unsafe or missing address clears what was there, instead of leaving a stale one behind.
  stashPendingLoginRedirect('/platform/knowledge-bases', storage)
  stashPendingLoginRedirect('https://evil.com', storage)
  assert.equal(storage.getItem(PENDING_LOGIN_REDIRECT_KEY), null)
  stashPendingLoginRedirect('/platform/knowledge-bases', storage)
  stashPendingLoginRedirect(undefined, storage)
  assert.equal(storage.getItem(PENDING_LOGIN_REDIRECT_KEY), null)

  // A tampered stash is still checked on the way out.
  assert.equal(consumePendingLoginRedirect(memoryStorage({ [PENDING_LOGIN_REDIRECT_KEY]: '//evil.com' })), null)
})

test('the guard and the login page pass every return address through safeRedirect', () => {
  const router = readFileSync(new URL('../router/index.ts', import.meta.url), 'utf8')
  // Logged-out users keep where they were heading (an invite link, typically).
  assert.match(
    router,
    /autoSetupAttempted = true[\s\S]*?next\(\{ path: '\/login', query: \{ redirect: to\.fullPath \} \}\)/
  )
  // Only the onboarding gate still sends a bare /login: its target is where a tenantless
  // login lands anyway, and a user with a workspace is bounced off it.
  assert.equal(router.match(/next\('\/login'\)/g)?.length, 1)
  assert.match(router, /if \(to\.path === '\/onboarding\/workspace'\)[\s\S]*?next\('\/login'\)/)
  assert.match(router, /if \(to\.path === '\/login' && authStore\.isLoggedIn\) \{\s*next\(safeRedirect\(to\.query\.redirect\) \?\?/)

  const login = readFileSync(new URL('../views/auth/Login.vue', import.meta.url), 'utf8')
  assert.match(login, /const loginRedirectTarget = \(\) =>\s*postLoginTarget\(safeRedirect\(route\.query\.redirect\) \?\? consumePendingLoginRedirect\(sessionStorage\), authStore\.hasValidTenant\)/)
  assert.match(login, /router\.replace\(loginRedirectTarget\(\)\)/)
  assert.match(login, /router\.replace\(postLoginTarget\(safeRedirect\(route\.query\.redirect\), authStore\.hasValidTenant\)\)/)
  assert.match(login, /stashPendingLoginRedirect\(route\.query\.redirect, sessionStorage\)\s*window\.location\.href = authorizationURL/)
  // The raw query is never followed and the default redirect is not hardcoded twice.
  assert.doesNotMatch(login, /router\.replace\((?:authStore\.hasValidTenant \?|route\.query)/)
  assert.equal(login.match(/consumePendingLoginRedirect\(sessionStorage\)/g)?.length, 1)

  const app = readFileSync(new URL('../App.vue', import.meta.url), 'utf8')
  assert.match(
    app,
    /router\.replace\(postLoginTarget\(consumePendingLoginRedirect\(sessionStorage\), authStore\.hasValidTenant\)\)/
  )
})
