import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

// Receivers cannot check another workspace's models, so the list shows the
// server's verdict on a shared agent's saved configuration (ready / issues)
// and keeps such an agent out of chats.
const source = readFileSync(new URL('./AgentList.vue', import.meta.url), 'utf8')

test('cards on every list carry the server verdict for shared agents', () => {
  // "全部 / 收藏 / 最近" build their cards from sharedAgents.
  assert.match(source, /ready: shared\.ready,/)
  assert.match(source, /ready: s\.ready,/)
  assert.match(
    source,
    /<t-tag v-if="!agent\.isMine && agent\.ready === false"[\s\S]*?\{\{ \$t\('organization\.share\.sharedNotReady'\) \}\}<\/t-tag>/,
  )
  // The space view lists what this workspace shared too, so it says so there.
  assert.match(
    source,
    /<t-tag v-if="shared\.ready === false"[\s\S]*?shared\.is_mine \? \$t\('organization\.share\.mineNotReady'\) : \$t\('organization\.share\.sharedNotReady'\)/,
  )
})

test('a shared agent that is not ready cannot be used in a chat', () => {
  assert.match(
    source,
    /const currentSharedAgentNotReady = computed\(\(\) => isSharedAgentNotReady\(currentSharedAgent\.value\)\)/,
  )
  assert.match(
    source,
    /<t-button theme="primary" block :disabled="currentSharedAgentNotReady"\s+@click="handleUseSharedAgentInChat\(currentSharedAgent\)">/,
  )
  assert.match(source, /<t-alert v-if="currentSharedAgentNotReady" theme="warning" class="shared-detail-not-ready"/)

  // The guard also holds when the function is called directly.
  const body = source.match(/async function handleUseSharedAgentInChat\(shared: SharedAgentInfo\) \{([\s\S]*?)\n\}/)?.[1]
  assert.ok(body, 'expected the handler')
  const refused = body.indexOf("MessagePlugin.warning(t('organization.share.sharedNotReady'))")
  assert.ok(refused >= 0, 'the user is told why')
  assert.ok(refused < body.indexOf('closeSharedAgentDetail()'), 'before anything else happens')
  assert.ok(refused < body.indexOf('createSessions({})'), 'and before a session is created')
})
