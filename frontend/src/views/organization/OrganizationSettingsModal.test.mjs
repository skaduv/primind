import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('./OrganizationSettingsModal.vue', import.meta.url), 'utf8')
// 遮罩层由共享壳 SettingsModalShell 渲染，overscroll 规则也随之搬到那里。
const shellSource = readFileSync(new URL('../../components/SettingsModalShell.vue', import.meta.url), 'utf8')

test('reviewing join requests goes through the store and refreshes modal data', () => {
  assert.match(
    source,
    /const refreshOrganizationAfterReview = async \(\) => \{\s*await Promise\.all\(\[\s*fetchOrgDetail\(\),\s*fetchMembers\(\)\s*\]\)\s*\}/
  )

  assert.match(source, /orgStore\.reviewOrganizationJoinRequest\(/)
  const refreshCalls = source.match(/await refreshOrganizationAfterReview\(\)/g) ?? []
  assert.equal(refreshCalls.length, 2)
})

test('organization settings use one outer content scroller and reset it on navigation', () => {
  assert.match(source, /ref="contentWrapperRef" class="content-wrapper"/)
  assert.match(source, /class="data-table-shell members-table-shell"/)
  assert.match(source, /contentWrapperRef\.value\?\.scrollTo\(\{ top: 0, behavior: 'auto' \}\)/)
  assert.match(source, /watch\(\(\) => props\.visible,[\s\S]*?void scrollContentToTop\(\)/)
  assert.match(source, /\}, \{ immediate: true \}\)\s*\n\s*watch\(\(\) => props\.orgId/)
  assert.match(source, /watch\(\(\) => props\.orgId,[\s\S]*?void scrollContentToTop\(\)/)
  assert.match(source, /watch\(currentSection,[\s\S]*?void scrollContentToTop\(\)/)
  assert.match(source, /\.members-table-shell\s*\{[\s\S]*?overflow: visible;[\s\S]*?\.t-table__content/)
})

test('organization settings lock and restore background scrolling', () => {
  assert.match(source, /document\.body\.style\.overflow = 'hidden'/)
  assert.match(source, /document\.body\.style\.overflow = previousBodyOverflow/)
  assert.match(source, /onBeforeUnmount\(\(\) => \{\s*unlockBackgroundScroll\(\)/)
  assert.match(shellSource, /\.settings-overlay\s*\{[\s\S]*?overscroll-behavior: none;/)
  assert.match(source, /\.content-wrapper\s*\{[\s\S]*?overscroll-behavior: contain;/)
})

function between(from, to) {
  const start = source.indexOf(from)
  const end = source.indexOf(to, start)
  assert.notEqual(start, -1, `start marker not found: ${from}`)
  assert.notEqual(end, -1, `end marker not found: ${to}`)
  return source.slice(start, end)
}

test('an expired invite code is flagged on the code, the link and the validity line', () => {
  // One expiry rule, shared with the remaining-validity text: an expiry time that is not in the future.
  assert.match(
    source,
    /const inviteCodeExpired = computed\(\(\) => \{\s*const at = inviteCodeExpiresAt\.value\s*if \(!at\) return false[\s\S]*?expiresAt <= Date\.now\(\)/
  )
  assert.match(source, /if \(inviteCodeExpired\.value\) return t\('organization\.settings\.remainingValidityExpired'\)/)
  assert.equal(source.match(/v-if="inviteCodeExpired"[^>]*class="invite-expired-tag"/g)?.length, 2)
  assert.match(source, /<div class="invite-code-box" :class="\{ 'is-expired': inviteCodeExpired \}">/)
  assert.match(source, /<div class="invite-link-box" :class="\{ 'is-expired': inviteCodeExpired \}">/)
  assert.match(source, /class="invite-remaining" :class="\{ 'is-expired': inviteCodeExpired \}"/)
})

test('copying an expired invite code or link asks first instead of copying', () => {
  const copyCode = between('const copyInviteCode = async', 'const copyInviteLink = async')
  const copyLink = between('const copyInviteLink = async', '// 邀请码已过期时')
  for (const [name, block, target] of [['code', copyCode, 'code'], ['link', copyLink, 'link']]) {
    assert.match(block, new RegExp(`if \\(inviteCodeExpired\\.value\\) \\{\\s*confirmExpiredInviteCopy\\('${target}'\\)\\s*return\\s*\\}`), name)
    // Exactly one direct copy per handler, reached only when the code is still valid.
    assert.equal(block.match(/copyWithToast\(/g)?.length, 1, name)
    assert.ok(block.indexOf('if (inviteCodeExpired.value)') < block.indexOf('copyWithToast('), name)
  }
})

test('refresh and copy stops when the refresh fails and copies the new value otherwise', () => {
  const confirm = between('const confirmExpiredInviteCopy', '// 刷新邀请码。返回是否刷新成功')
  assert.match(confirm, /DialogPlugin\.confirm\(\{/)
  assert.match(confirm, /header: t\('organization\.settings\.inviteCodeExpiredTitle'\)/)
  assert.match(confirm, /body: t\('organization\.settings\.inviteCodeExpiredCopyBody'\)/)
  assert.match(confirm, /confirmBtn: t\('organization\.settings\.refreshAndCopy'\)/)
  assert.match(confirm, /cancelBtn: t\('common\.cancel'\)/)
  assert.match(
    confirm,
    /const refreshed = await refreshInviteCode\(\)\s*if \(!refreshed\) return\s*await copyWithToast\(target === 'link' \? inviteLink\.value : inviteCode\.value, 'common\.copied'\)/
  )

  const refresh = between('const refreshInviteCode = async', 'const handleValidityChange')
  assert.match(refresh, /^const refreshInviteCode = async \(\): Promise<boolean> => \{/)
  // The expiry stamp is reloaded before success is reported, so the "expired" flag clears at once.
  assert.match(refresh, /await fetchOrgDetail\(\)\s*return true/)
  assert.match(refresh, /return false[\s\S]*?return false/)
})
