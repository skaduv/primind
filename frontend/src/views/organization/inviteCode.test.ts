import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  INVITE_CODE_EXPIRED,
  INVITE_CODE_NOT_FOUND,
  extractInviteCode,
  inviteErrorKey,
  inviteFailureMessage,
  knownInviteErrorKey,
} from './inviteCode.ts'

const listSource = readFileSync(new URL('./OrganizationList.vue', import.meta.url), 'utf8')
const storeSource = readFileSync(new URL('../../stores/organization.ts', import.meta.url), 'utf8')

const FALLBACK = 'organization.invite.joinFailed'
const CODE = '6e9b738f4f62c1e0'

function section(source: string, from: string, to: string): string {
  const start = source.indexOf(from)
  const end = source.indexOf(to, start)
  assert.notEqual(start, -1, `start marker not found: ${from}`)
  assert.notEqual(end, -1, `end marker not found: ${to}`)
  return source.slice(start, end)
}

/** Stand-in for vue-i18n's t(): returns the key so the tests can tell keys apart. */
const keyAsText = (key: string) => key

// ---- inviteErrorKey ----

test('inviteErrorKey names the typed invite failures', () => {
  assert.equal(inviteErrorKey({ code: INVITE_CODE_EXPIRED, status: 404 }, FALLBACK), 'organization.invite.codeExpired')
  assert.equal(inviteErrorKey({ code: INVITE_CODE_NOT_FOUND, status: 404 }, FALLBACK), 'organization.invite.codeNotFound')
  // The typed code wins even when the status alone would say less.
  assert.equal(inviteErrorKey({ code: INVITE_CODE_EXPIRED }, FALLBACK), 'organization.invite.codeExpired')
})

test('inviteErrorKey falls back by status against an older backend', () => {
  // The old backend answered every lookup failure with 404 and the generic not-found code.
  assert.equal(inviteErrorKey({ code: 1003, status: 404, message: 'Invalid invite code' }, FALLBACK), 'organization.invite.invalidCode')
  assert.equal(inviteErrorKey({ status: 404 }, FALLBACK), 'organization.invite.invalidCode')
  assert.equal(inviteErrorKey({ status: 500 }, FALLBACK), 'organization.invite.previewFailed')
  assert.equal(inviteErrorKey({ status: 503 }, FALLBACK), 'organization.invite.previewFailed')
  // No response at all is a network error.
  assert.equal(inviteErrorKey({ message: 'Network Error' }, FALLBACK), 'organization.invite.previewFailed')
  assert.equal(inviteErrorKey({}, FALLBACK), 'organization.invite.previewFailed')
})

test('inviteErrorKey leaves everything else to the caller', () => {
  // Space full or request already pending: the backend sends its own message and a non-invite code.
  assert.equal(inviteErrorKey({ code: 1010, status: 400, message: '该空间成员已满，无法加入' }, FALLBACK), FALLBACK)
  assert.equal(inviteErrorKey({ status: 400 }, FALLBACK), FALLBACK)
  assert.equal(inviteErrorKey({ status: 403 }, FALLBACK), FALLBACK)
})

test('knownInviteErrorKey tells a service failure from a fallback even when both share a key', () => {
  // The preview page passes previewFailed as its fallback, which is also the
  // key for 5xx. Comparing keys alone cannot tell "500" from "something else".
  const serviceFailure = { code: 1007, status: 500, message: 'Failed to look up invite code' }
  assert.equal(inviteErrorKey(serviceFailure, 'organization.invite.previewFailed'), 'organization.invite.previewFailed')
  assert.equal(knownInviteErrorKey(serviceFailure), 'organization.invite.previewFailed')
  assert.equal(knownInviteErrorKey({ code: 1010, status: 400, message: '该空间成员已满，无法加入' }), null)
  assert.equal(knownInviteErrorKey({ status: 403 }), null)
})

// ---- inviteFailureMessage ----

test('a service failure is translated, never shown as the backend English message', () => {
  // Found end to end: the new backend answers a failed lookup with 500 and a
  // business code; that used to slip through as "Failed to look up invite code".
  assert.equal(
    inviteFailureMessage({ code: 1007, status: 500, message: 'Failed to look up invite code' }, 'organization.invite.previewFailed', keyAsText),
    'organization.invite.previewFailed',
  )
  assert.equal(
    inviteFailureMessage({ message: 'Network Error' }, 'organization.invite.previewFailed', keyAsText),
    'organization.invite.previewFailed',
  )
  assert.equal(
    inviteFailureMessage({ code: 2401, status: 404, message: 'Invalid invite code' }, FALLBACK, keyAsText),
    'organization.invite.codeExpired',
  )
  assert.equal(
    inviteFailureMessage({ code: 1003, status: 404, message: 'Invalid invite code' }, FALLBACK, keyAsText),
    'organization.invite.invalidCode',
  )
})

test('other failures keep the backend message and fall back to the translation when there is none', () => {
  assert.equal(
    inviteFailureMessage({ code: 1010, status: 400, message: '该空间成员已满，无法加入' }, FALLBACK, keyAsText),
    '该空间成员已满，无法加入',
  )
  assert.equal(inviteFailureMessage({ status: 400 }, FALLBACK, keyAsText), FALLBACK)
  // No status at all is a network error, which is a known failure (C3), not a fallback.
  assert.equal(inviteFailureMessage({}, FALLBACK, keyAsText), 'organization.invite.previewFailed')
})

// ---- extractInviteCode ----

test('extractInviteCode accepts a bare code, with or without whitespace', () => {
  assert.equal(extractInviteCode(CODE), CODE)
  assert.equal(extractInviteCode(`  ${CODE} \n`), CODE)
  assert.equal(extractInviteCode(''), '')
  assert.equal(extractInviteCode('   '), '')
})

test('extractInviteCode takes the code out of a whole invite link', () => {
  assert.equal(extractInviteCode(`http://localhost:8080/join?code=${CODE}`), CODE)
  assert.equal(extractInviteCode(`/join?code=${CODE}`), CODE)
  assert.equal(extractInviteCode(`https://primind.example.com/join?code=${CODE}`), CODE)
  assert.equal(extractInviteCode(`/platform/organizations?invite_code=${CODE}`), CODE)
  assert.equal(extractInviteCode(`https://primind.example.com/platform/organizations?invite_code=${CODE}`), CODE)
  assert.equal(extractInviteCode(`?invite_code=${CODE}`), CODE)
  assert.equal(extractInviteCode(`  https://primind.example.com/join?code=${CODE}&x=1#top `), CODE)
})

test('extractInviteCode returns text it cannot parse trimmed, as is', () => {
  assert.equal(extractInviteCode('not a code'), 'not a code')
  assert.equal(extractInviteCode('  no code here  '), 'no code here')
  assert.equal(extractInviteCode('http://'), 'http://')
  assert.equal(extractInviteCode('/join?other=1'), '/join?other=1')
  assert.equal(extractInviteCode('/join?code='), '/join?code=')
})

// ---- the join window wires it up (source assertions, same style as OrganizationSettingsModal.test.mjs) ----

test('a failed link preview leaves the code in the input so 「查看」 can retry it', () => {
  const preview = section(listSource, 'async function handleInvitePreview', '// 确认加入组织')
  assert.match(
    preview,
    /invitePreviewError\.value = inviteFailureMessage\(result, 'organization\.invite\.previewFailed', t\)[^\n]*\n(?:[^\n]*\n)*?\s*joinInputCode\.value = code/
  )
  // The catch path backfills too, so a retry never depends on which branch failed.
  assert.match(preview, /invitePreviewError\.value = t\('organization\.invite\.previewFailed'\)\s*joinInputCode\.value = code/)
})

test('the join window never shows the backend message for a failed preview', () => {
  const preview = section(listSource, 'async function handleInvitePreview', '// 确认加入组织')
  assert.doesNotMatch(preview, /result\.message/)
  assert.doesNotMatch(preview, /e\?\.message/)
  assert.doesNotMatch(preview, /organization\.invite\.invalidCode/)
})

test('the input takes a whole invite link and both join failures go through the error mapping', () => {
  const fromInput = section(listSource, 'async function doPreviewFromInput', '// 关闭邀请预览弹框')
  assert.match(fromInput, /const code = extractInviteCode\(joinInputCode\.value \|\| ''\)/)
  assert.match(fromInput, /await handleInvitePreview\(code\)/)
  assert.doesNotMatch(listSource, /joinInputCode\.value\?\.trim\(\)/)
  // A link is longer than 32 characters; the old input limit truncated it.
  assert.doesNotMatch(listSource, /v-model="joinInputCode"[^>]*:maxlength="32"/)

  const confirm = section(listSource, 'async function confirmJoinOrganization', '// 从输入步骤点击「预览」')
  assert.equal(confirm.match(/MessagePlugin\.error\(inviteFailureMessage\(/g)?.length, 2)
  // The direct join reads the code and status the store kept.
  assert.match(confirm, /code: orgStore\.errorCode \?\? undefined,\s*status: orgStore\.errorStatus \?\? undefined,/)
  assert.doesNotMatch(confirm, /MessagePlugin\.error\((?:orgStore\.error|result\.message|e\?\.message) \|\|/)
})

test('the page has no error mapping of its own: every invite failure goes through the helper', () => {
  // The mapping lives in inviteCode.ts where it is unit-tested; a copy in the
  // page would drift (the first version showed the English 500 message).
  assert.doesNotMatch(listSource, /function inviteFailureText/)
  assert.doesNotMatch(listSource, /inviteErrorKey\(/)
  assert.equal(listSource.match(/inviteFailureMessage\(/g)?.length, 3, 'preview, join request, direct join')
})

test('the store keeps the code and status of a failed join next to its message', () => {
  const join = section(storeSource, 'async function join(inviteCode', 'async function leave(id: string)')
  assert.match(join, /errorCode\.value = null\s*errorStatus\.value = null/)
  assert.match(join, /error\.value = response\.message \|\| 'Failed to join organization'\s*errorCode\.value = response\.code \?\? null\s*errorStatus\.value = response\.status \?\? null/)
  assert.match(storeSource, /error,\s*errorCode,\s*errorStatus,/)
  assert.match(storeSource, /error\.value = null\s*errorCode\.value = null\s*errorStatus\.value = null/)
})
