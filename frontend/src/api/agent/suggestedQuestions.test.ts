import assert from 'node:assert/strict'
import { after, before, beforeEach, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { createServer, type ViteDevServer } from 'vite'
import { createPinia } from 'pinia'
import { effectScope, nextTick, watch } from 'vue'

// Loads the real settings store and agent API with the HTTP client replaced by
// a recorder, so the request a selection produces can be read off verbatim.
let server: ViteDevServer
let useSettingsStore: typeof import('../../stores/settings').useSettingsStore
let getSuggestedQuestions: typeof import('./index').getSuggestedQuestions
const requests: Array<{ url: string }> = []
const savedStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')
const items = new Map<string, string>()

before(async () => {
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
    getItem: (key: string) => items.get(key) ?? null,
    setItem: (key: string, value: string) => items.set(key, value),
    removeItem: (key: string) => items.delete(key),
  } })
  server = await createServer({
    configFile: false,
    plugins: [{ name: 'record-request-test', enforce: 'pre',
      resolveId(id) { if (id.endsWith('/utils/request')) return '\0record-request' },
      load(id) {
        if (id !== '\0record-request') return
        return `const record = (url) => (globalThis.__suggestionRequests.push({ url }),
          Promise.resolve({ data: { questions: [] } }));
          export { record as get, record as post, record as put, record as del };`
      },
    }],
    optimizeDeps: { noDiscovery: true, entries: [] },
    resolve: { alias: { '@': fileURLToPath(new URL('../../', import.meta.url)) } },
    server: { middlewareMode: true, hmr: false }, appType: 'custom',
  })
  ;({ useSettingsStore } = await server.ssrLoadModule('/src/stores/settings.ts'))
  ;({ getSuggestedQuestions } = await server.ssrLoadModule('/src/api/agent/index.ts'))
})

beforeEach(() => {
  items.clear()
  requests.length = 0
  ;(globalThis as any).__suggestionRequests = requests
})

after(async () => {
  await server?.close()
  if (savedStorage) Object.defineProperty(globalThis, 'localStorage', savedStorage)
  else Reflect.deleteProperty(globalThis, 'localStorage')
})

const scope = () => effectScope(true)

test('选中共享智能体：推荐请求带上来源空间', async () => {
  const store = useSettingsStore(createPinia())
  store.selectAgent('agent-1', '84')

  assert.deepEqual(store.selectedAgentRef, { id: 'agent-1', sourceTenantId: '84' })
  await getSuggestedQuestions(store.selectedAgentRef, { knowledge_base_ids: ['kb-1', 'kb-2'], limit: 6 })

  assert.deepEqual(requests.map((r) => r.url), [
    '/api/v1/agents/agent-1/suggested-questions?agent_source_tenant_id=84&knowledge_base_ids=kb-1%2Ckb-2&limit=6',
  ])
})

test('选中本空间智能体：推荐请求不带来源空间', async () => {
  const store = useSettingsStore(createPinia())
  store.selectAgent('agent-1')
  await getSuggestedQuestions(store.selectedAgentRef, { limit: 3 })
  store.selectAgent('builtin-quick-answer', '')
  assert.deepEqual(store.selectedAgentRef, { id: 'builtin-quick-answer', sourceTenantId: null })
  await getSuggestedQuestions(store.selectedAgentRef, store.getSuggestedQuestionsParams())

  assert.deepEqual(requests.map((r) => r.url), [
    '/api/v1/agents/agent-1/suggested-questions?limit=3',
    '/api/v1/agents/builtin-quick-answer/suggested-questions',
  ])
})

// 首页和会话页都监听「ID + 来源空间」：两个智能体 ID 相同、来源不同时，
// 每次切换都要重新请求，只看 ID 会漏掉切换。
test('两个智能体 ID 相同、来源不同，互相切换：每次切换都重新请求', async () => {
  const store = useSettingsStore(createPinia())
  const watcher = scope()
  watcher.run(() => watch(
    () => store.selectedAgentRef,
    (agent) => { void getSuggestedQuestions(agent, store.getSuggestedQuestionsParams()) },
    { immediate: true },
  ))

  store.selectAgent('agent-1', '84')
  await nextTick()
  store.selectAgent('agent-1', '85')
  await nextTick()
  store.selectAgent('agent-1')
  await nextTick()
  store.selectAgent('agent-1', '84')
  await nextTick()
  store.selectAgent('agent-2', '84')
  await nextTick()
  watcher.stop()

  assert.deepEqual(requests.map((r) => r.url), [
    '/api/v1/agents/builtin-quick-answer/suggested-questions',
    '/api/v1/agents/agent-1/suggested-questions?agent_source_tenant_id=84',
    '/api/v1/agents/agent-1/suggested-questions?agent_source_tenant_id=85',
    '/api/v1/agents/agent-1/suggested-questions',
    '/api/v1/agents/agent-1/suggested-questions?agent_source_tenant_id=84',
    '/api/v1/agents/agent-2/suggested-questions?agent_source_tenant_id=84',
  ])
})

// 只看 ID 的监听漏掉「同 ID 换来源」的切换，这正是要带上来源空间的原因。
test('只监听智能体 ID 时，同 ID 换来源不会触发请求', async () => {
  const store = useSettingsStore(createPinia())
  const watcher = scope()
  watcher.run(() => watch(
    () => store.selectedAgentId,
    (agentId) => { void getSuggestedQuestions({ id: agentId, sourceTenantId: null }) },
  ))

  store.selectAgent('agent-1', '84')
  await nextTick()
  store.selectAgent('agent-1', '85')
  await nextTick()
  watcher.stop()

  assert.equal(requests.length, 1)
})
