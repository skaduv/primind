import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { runInNewContext } from 'node:vm'
import test from 'node:test'
import { compileScript, parse } from '@vue/compiler-sfc'
import ts from 'typescript'
import * as vue from 'vue'
import zhCN from '../i18n/locales/zh-CN.ts'

const require = createRequire(import.meta.url)
const renderer = vue.createRenderer<any, any>({
  createElement: () => ({}), createText: () => ({}), createComment: () => ({}),
  insert() {}, remove() {}, setElementText() {}, setText() {}, patchProp() {},
  parentNode: () => null, nextSibling: () => null,
})

function message(path: string): string {
  let node: any = zhCN
  for (const part of path.split('.')) node = node?.[part]
  return typeof node === 'string' ? node : path
}

const t = (key: string, params?: Record<string, unknown>) => {
  let text = message(key)
  for (const [name, value] of Object.entries(params ?? {})) text = text.replaceAll(`{${name}}`, String(value))
  return text
}

function fixture() {
  const orgStore: any = { sharedAgents: [] as any[] }
  const events: Array<{ name: string; args: unknown[] }> = []
  const modules: Record<string, any> = {
    vue,
    'vue-i18n': { useI18n: () => ({ t, locale: vue.ref('zh-CN') }) },
    'vue-router': { useRouter: () => ({ push() {} }) },
    'tdesign-vue-next': { Icon: {}, Tooltip: {} },
    '@/api/agent': {
      BUILTIN_QUICK_ANSWER_ID: 'builtin-quick-answer', BUILTIN_SMART_REASONING_ID: 'builtin-smart-reasoning',
    },
    '@/stores/organization': { useOrganizationStore: () => orgStore },
    '@/stores/settings': { useSettingsStore: () => ({ selectedAgentSourceTenantId: null }) },
    '@/stores/chatResources': {
      useChatResourcesStore: () => ({ webSearchProviders: [], ensureWebSearchProviders() {} }),
    },
    '@/utils/zoom': {
      getRootZoom: () => 1, rectToCssPx: (rect: any) => rect,
      cssViewportSize: () => ({ width: 1280, height: 800 }),
    },
  }
  function evaluate(code: string) {
    const exports: any = {}
    const compiled = ts.transpileModule(code, {
      compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
    }).outputText
    runInNewContext(compiled, {
      exports, setTimeout, clearTimeout, console,
      require(name: string) {
        if (modules[name]) return modules[name]
        if (name.endsWith('.vue')) return { default: {} }
        if (name.startsWith('@/')) return require('../' + name.slice(2))
        return require(name)
      },
    })
    return exports
  }
  function mount(props: Record<string, any>) {
    const { descriptor } = parse(readFileSync(new URL('./AgentSelector.vue', import.meta.url), 'utf8'))
    const script = compileScript(descriptor, { id: 'selector-test' }).content
      .replace('__expose();', '')
      .replace('return __returned__', '__expose(__returned__); return __returned__')
    const component = evaluate(script).default
    component.render = () => null
    const instance = vue.ref<any>()
    const app = renderer.createApp({ render: () => vue.h(component, { ...props, ref: instance }) })
    app.mount({})
    return { vm: instance.value, unmount: () => app.unmount() }
  }
  return { mount, orgStore, events }
}

const completeConfig = {
  model_id: 'chat-1', rerank_model_id: 'rerank-1',
  kb_selection_mode: 'all', allowed_tools: ['search_knowledge'], agent_mode: 'smart-reasoning',
}

function sharedAgent(overrides: Record<string, unknown>) {
  return {
    agent: { id: 'shared-1', name: '共享智能体', config: completeConfig },
    share_id: 'share-1', organization_id: 'org-1', org_name: '研发空间', permission: 'viewer',
    source_tenant_id: 84, shared_at: '2026-11-26T00:00:00Z', web_search_ready: false,
    ...overrides,
  }
}

function mountSelector(f: ReturnType<typeof fixture>) {
  const { vm, unmount } = f.mount({
    visible: true, currentAgentId: '',
    onSelect: (...args: unknown[]) => { f.events.push({ name: 'select', args }) },
    onNotReady: (...args: unknown[]) => { f.events.push({ name: 'not-ready', args }) },
    onClose: () => { f.events.push({ name: 'close', args: [] }) },
  })
  return { vm, unmount }
}

const eventsNamed = (f: ReturnType<typeof fixture>, name: string) => f.events.filter(event => event.name === name)

test('a shared agent the server marked not ready cannot be selected; the reason is passed on', async () => {
  const f = fixture()
  const issues = [{ code: 'missing_rerank_model', field: 'config.rerank_model_id' }]
  f.orgStore.sharedAgents = [
    sharedAgent({ agent: { id: 'shared-not-ready', name: '未就绪', config: completeConfig }, ready: false, issues }),
    sharedAgent({ agent: { id: 'shared-ready', name: '就绪', config: completeConfig }, ready: true, source_tenant_id: 85 }),
  ]
  const { vm, unmount } = mountSelector(f)
  try {
    await vue.nextTick()
    const [notReady, ready] = vm.sharedAgentsList
    assert.equal(notReady.agent.id, 'shared-not-ready')

    // The row says so, even though its saved configuration looks complete:
    // a receiver cannot check the other workspace's models. (The component
    // builds its list in another realm, so copy it before comparing.)
    assert.deepEqual([...vm.getAgentNotReadyLabels(notReady.agent, '84')], [message('organization.share.sharedNotReady')])
    assert.deepEqual([...vm.getAgentNotReadyLabels(ready.agent, '85')], [])

    vm.selectSharedAgent(notReady)
    assert.equal(eventsNamed(f, 'select').length, 0, 'nothing is selected')
    const refused = eventsNamed(f, 'not-ready')
    assert.equal(refused.length, 1)
    assert.equal((refused[0].args[0] as any).id, 'shared-not-ready')
    assert.deepEqual(refused[0].args[1], ['启用知识库检索时需要 ReRank 模型'], 'the chat bar gets the exact missing item')
    assert.equal(refused[0].args[3], '84')

    vm.selectSharedAgent(ready)
    const selected = eventsNamed(f, 'select')
    assert.equal(selected.length, 1)
    assert.equal((selected[0].args[0] as any).id, 'shared-ready')
    assert.equal(selected[0].args[1], '85')
  } finally { unmount() }
})

test('without a server verdict (older backend) the local rules still apply', async () => {
  const f = fixture()
  f.orgStore.sharedAgents = [
    sharedAgent({ agent: { id: 'shared-no-verdict', name: '无结论', config: completeConfig } }),
    sharedAgent({
      agent: { id: 'shared-no-model', name: '缺模型', config: { ...completeConfig, model_id: '' } },
      source_tenant_id: 85,
    }),
  ]
  const { vm, unmount } = mountSelector(f)
  try {
    await vue.nextTick()
    const [noVerdict, noModel] = vm.sharedAgentsList
    assert.deepEqual(vm.getAgentNotReadyLabels(noVerdict.agent, '84'), [], 'complete configuration is selectable')
    assert.equal(vm.getAgentNotReadyLabels(noModel.agent, '85').length, 1, 'a missing model ID is still caught locally')

    vm.selectSharedAgent(noVerdict)
    assert.equal(eventsNamed(f, 'select').length, 1)
    vm.selectSharedAgent(noModel)
    assert.equal(eventsNamed(f, 'select').length, 1)
    assert.equal(eventsNamed(f, 'not-ready').length, 1)
  } finally { unmount() }
})
