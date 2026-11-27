import assert from 'node:assert/strict'
import test from 'node:test'

import enUS from '../../i18n/locales/en-US.ts'
import jaJP from '../../i18n/locales/ja-JP.ts'
import koKR from '../../i18n/locales/ko-KR.ts'
import ruRU from '../../i18n/locales/ru-RU.ts'
import zhCN from '../../i18n/locales/zh-CN.ts'
import zhTW from '../../i18n/locales/zh-TW.ts'

const LOCALES = { 'en-US': enUS, 'ja-JP': jaJP, 'ko-KR': koKR, 'ru-RU': ruRU, 'zh-CN': zhCN, 'zh-TW': zhTW }

/** Keys BF-06 added: two member-facing invite messages and the admin "refresh and copy" dialog. */
const NEW_KEYS = [
  'organization.invite.codeExpired',
  'organization.invite.codeNotFound',
  'organization.settings.inviteCodeExpiredTitle',
  'organization.settings.inviteCodeExpiredCopyBody',
  'organization.settings.refreshAndCopy',
]

/** Keys BF-06 relies on that already existed. */
const REUSED_KEYS = [
  'organization.invite.invalidCode',
  'organization.invite.previewFailed',
  'organization.invite.joinFailed',
  'organization.invite.requestFailed',
  'organization.settings.remainingValidityExpired',
]

function at(bundle: unknown, path: string): unknown {
  return path.split('.').reduce<unknown>((node, key) => (
    node !== null && typeof node === 'object' ? (node as Record<string, unknown>)[key] : undefined
  ), bundle)
}

test('every invite message the fix can show exists in all six locales', () => {
  for (const [name, bundle] of Object.entries(LOCALES)) {
    for (const path of [...NEW_KEYS, ...REUSED_KEYS]) {
      const value = at(bundle, path)
      assert.equal(typeof value, 'string', `${name}: missing ${path}`)
      assert.notEqual((value as string).trim(), '', `${name}: empty ${path}`)
    }
  }
})

test('the zh-CN messages tell expired from invalid and never fall back to the English original', () => {
  assert.equal(zhCN.organization.invite.codeExpired, '邀请码已过期，请向空间管理员索取新的邀请链接')
  assert.equal(zhCN.organization.invite.codeNotFound, '邀请码无效，请核对邀请码，或向空间管理员索取新的邀请链接')
  // Old backend: one message covers both cases.
  assert.equal(zhCN.organization.invite.invalidCode, '邀请码无效或已过期')
  assert.equal(zhCN.organization.invite.previewFailed, '预览失败，请稍后重试')
  assert.equal(zhCN.organization.invite.joinFailed, '加入失败，请稍后重试')
  assert.equal(zhCN.organization.invite.requestFailed, '申请提交失败，请稍后重试')

  const shownWhenAFailureIsMapped = [
    zhCN.organization.invite.codeExpired,
    zhCN.organization.invite.codeNotFound,
    zhCN.organization.invite.invalidCode,
    zhCN.organization.invite.previewFailed,
  ]
  for (const message of shownWhenAFailureIsMapped) {
    assert.doesNotMatch(message, /Invalid invite code|Failed to/i, message)
  }
})

test('the admin dialog wording matches the spec', () => {
  assert.equal(zhCN.organization.settings.inviteCodeExpiredCopyBody, '当前邀请码已过期，复制出去也无法使用。')
  assert.equal(zhCN.organization.settings.refreshAndCopy, '刷新并复制')
  assert.equal(zhCN.organization.settings.inviteCodeExpiredTitle, '邀请码已过期')
  // The tag reuses the validity line's wording.
  assert.equal(zhCN.organization.settings.remainingValidityExpired, '已过期')
})
