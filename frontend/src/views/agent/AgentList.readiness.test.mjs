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

// Saving an agent may change the "配置不完整" verdict on the space view's cards,
// which come from their own endpoint.
test('saving in the space view refreshes its cards; a stale answer never wins', async () => {
  const success = source.match(/const handleEditorSuccess = \(agent\?: CustomAgent\) => \{([\s\S]*?)\n\}/)?.[1]
  assert.ok(success, 'expected handleEditorSuccess')
  assert.match(
    success,
    /if \(spaceSelectionOrgId\.value\) void fetchSpaceAgents\(spaceSelection\.value, \{ silent: true \}\)/,
  )

  const start = source.indexOf('let spaceAgentsRequest = 0')
  const end = source.indexOf('// 选中空间时请求该空间内全部智能体', start)
  assert.ok(start >= 0 && end > start, 'expected fetchSpaceAgents')
  const ts = (await import('typescript')).default
  const vm = await import('node:vm')
  const { ref } = await import('vue')
  const pending = []
  const spaceAgentsList = ref([])
  const spaceAgentsLoading = ref(false)
  const spaceAgentCountByOrg = ref({})
  const { fetchSpaceAgents } = vm.runInNewContext(
    ts.transpile(`${source.slice(start, end)}\n({ fetchSpaceAgents })`),
    {
      spaceAgentsList, spaceAgentsLoading, spaceAgentCountByOrg,
      listOrganizationSharedAgents: (orgId) => new Promise((resolve) => pending.push({ orgId, resolve })),
    },
  )
  const answer = (index, data) => pending[index].resolve({ success: true, data })

  // A space is opened, then the editor saves: the save's answer is the newer one.
  const opened = fetchSpaceAgents('org-1')
  assert.equal(spaceAgentsLoading.value, true)
  const refreshed = fetchSpaceAgents('org-1', { silent: true })
  answer(1, [{ id: 'a', ready: false }])
  await refreshed
  answer(0, [{ id: 'a', ready: true }])
  await opened
  assert.equal(spaceAgentsList.value[0].ready, false, 'the older answer is dropped')
  assert.equal(spaceAgentsLoading.value, false)

  // A silent refresh that fails keeps the cards on screen.
  const failed = fetchSpaceAgents('org-1', { silent: true })
  pending[2].resolve({ success: false })
  await failed
  assert.equal(spaceAgentsList.value.length, 1)
})
