import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { runInNewContext } from 'node:vm'
import test from 'node:test'
import { compileScript, parse } from '@vue/compiler-sfc'
import ts from 'typescript'
import * as vue from 'vue'
import zhCN from '../i18n/locales/zh-CN.ts'
import { agentReadinessHighlightField } from '../utils/agent-share-readiness.ts'

const require = createRequire(import.meta.url)
const renderer = vue.createRenderer<any, any>({
  createElement: () => ({}), createText: () => ({}), createComment: () => ({}),
  insert() {}, remove() {}, setElementText() {}, setText() {}, patchProp() {},
  parentNode: () => null, nextSibling: () => null,
})

// Texts come from the real zh-CN bundle, so the assertions check what a
// Chinese-speaking user actually sees.
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
  const toasts: Array<{ kind: string; text: string }> = []
  const shareCalls: Array<Record<string, unknown>> = []
  // What POST /agents/:id/shares answers next; a test reassigns it.
  const responses: { share: unknown } = { share: { success: true, data: { id: 'share-1' } } }
  const orgStore: any = {
    organizations: [{ id: 'org-1', name: '研发空间', is_owner: true, my_role: 'admin' }],
    async fetchOrganizations() {},
    async shareAgent(agentId: string, request: Record<string, unknown>) {
      shareCalls.push({ agentId, ...request })
      return responses.share
    },
  }
  const toast = (kind: string) => (text: string) => { toasts.push({ kind, text }) }
  const modules: Record<string, any> = {
    vue,
    'vue-i18n': { useI18n: () => ({ t, locale: vue.ref('zh-CN') }) },
    'tdesign-vue-next': {
      MessagePlugin: { success: toast('success'), error: toast('error'), warning: toast('warning') },
    },
    '@/stores/organization': { useOrganizationStore: () => orgStore },
    '@/api/organization': { listAgentShares: async () => ({ success: true, data: { shares: [] } }) },
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
    const { descriptor } = parse(readFileSync(new URL('./AgentShareSettings.vue', import.meta.url), 'utf8'))
    const script = compileScript(descriptor, { id: 'share-test' }).content
      .replace('__expose();', '')
      .replace('return __returned__', '__expose(__returned__); return __returned__')
    const component = evaluate(script).default
    component.render = () => null
    const instance = vue.ref<any>()
    const app = renderer.createApp({ render: () => vue.h(component, { ...props, ref: instance }) })
    app.mount({})
    return { vm: instance.value, unmount: () => app.unmount() }
  }
  return { mount, toasts, shareCalls, orgStore, responses }
}

async function settle() {
  await new Promise<void>(resolve => setImmediate(resolve))
  await vue.nextTick()
}

function mountSharePanel(
  f: ReturnType<typeof fixture>,
  options: { dirty?: boolean; saveDraft?: () => Promise<boolean> } = {},
) {
  const events: Array<{ name: string; args: unknown[] }> = []
  const props = vue.reactive({
    agentId: 'agent-1',
    agent: { id: 'agent-1', name: 'Agent', is_builtin: false, config: {} },
    isDirty: () => options.dirty ?? false,
    saveDraft: options.saveDraft,
    onConfigure: (...args: unknown[]) => { events.push({ name: 'configure', args }) },
  })
  const mounted = f.mount(props)
  return { ...mounted, props, events }
}

test('with unsaved edits, "共享" only asks: no request, the selection stays', async () => {
  const f = fixture()
  const saves: string[] = []
  const { vm, unmount } = mountSharePanel(f, {
    dirty: true, saveDraft: async () => { saves.push('save'); return true },
  })
  try {
    await settle()
    vm.selectedOrgId = 'org-1'
    vm.addPopupVisible = true

    await vm.handleShare()

    assert.equal(vm.confirmingUnsaved, true, 'the popup turns into a confirmation')
    assert.equal(f.shareCalls.length, 0, 'no share request is sent')
    assert.deepEqual(saves, [], 'nothing is saved without the user asking')
    assert.equal(vm.selectedOrgId, 'org-1', 'the chosen space is still shown')
    assert.equal(vm.addPopupVisible, true)

    // Choosing another space also leaves the confirmation.
    vm.selectedOrgId = 'org-2'
    assert.equal(vm.confirmingUnsaved, false)

    vm.selectedOrgId = 'org-1'
    await vm.handleShare()
    assert.equal(vm.confirmingUnsaved, true)
    vm.cancelShareWithUnsaved() // "取消"
    assert.equal(vm.confirmingUnsaved, false)
    assert.equal(vm.selectedOrgId, 'org-1')
    assert.equal(f.shareCalls.length, 0)
  } finally { unmount() }
})

test('"保存并共享" saves first and shares only after the save succeeded', async () => {
  const f = fixture()
  const order: string[] = []
  const share = f.orgStore.shareAgent
  f.orgStore.shareAgent = async (...args: Parameters<typeof share>) => {
    order.push('share')
    return share(...args)
  }
  const { vm, unmount } = mountSharePanel(f, {
    dirty: true, saveDraft: async () => { order.push('save'); return true },
  })
  try {
    await settle()
    vm.selectedOrgId = 'org-1'
    await vm.handleShare()
    assert.equal(vm.confirmingUnsaved, true)

    await vm.saveAndShare()

    assert.deepEqual(order, ['save', 'share'])
    assert.equal(f.shareCalls.length, 1)
    assert.equal(f.shareCalls[0].organization_id, 'org-1')
    assert.equal(f.shareCalls[0].agentId, 'agent-1')
    assert.equal(vm.addPopupVisible, false)
    assert.equal(vm.selectedOrgId, '')
    assert.deepEqual(f.toasts.map(toast => toast.kind), ['success'])
  } finally { unmount() }
})

test('"保存并共享" stops when the save fails: no share request', async () => {
  const f = fixture()
  const order: string[] = []
  const { vm, unmount } = mountSharePanel(f, {
    dirty: true, saveDraft: async () => { order.push('save'); return false },
  })
  try {
    await settle()
    vm.addPopupVisible = true
    vm.selectedOrgId = 'org-1'
    await vm.handleShare()

    await vm.saveAndShare()

    assert.deepEqual(order, ['save'])
    assert.equal(f.shareCalls.length, 0)
    assert.equal(vm.selectedOrgId, 'org-1', 'the selection survives a failed save')
    assert.equal(vm.confirmingUnsaved, false)
    // A failed validation moves the editor to another section; the popup is
    // teleported to the page and would float over it.
    assert.equal(vm.addPopupVisible, false, 'the popup closes when the save fails')
  } finally { unmount() }
})

test('without unsaved edits the share request goes out directly', async () => {
  const f = fixture()
  const saves: string[] = []
  const { vm, unmount } = mountSharePanel(f, {
    dirty: false, saveDraft: async () => { saves.push('save'); return true },
  })
  try {
    await settle()
    vm.selectedOrgId = 'org-1'

    await vm.handleShare()

    assert.equal(f.shareCalls.length, 1)
    assert.equal(vm.confirmingUnsaved, false, 'no confirmation is shown')
    assert.deepEqual(saves, [])
    assert.deepEqual(f.toasts, [{ kind: 'success', text: message('organization.share.shareSuccess') }])
  } finally { unmount() }
})

test('a refusal that names the missing rerank model is shown in Chinese with a way to the field', async () => {
  const f = fixture()
  const issues = [{ code: 'missing_rerank_model', field: 'config.rerank_model_id' }]
  f.responses.share = {
    success: false,
    message: 'Agent is not fully configured. Please set the chat model, and set the rerank model if the knowledge_search tool is enabled in agent settings.',
    error: { code: 1010, details: { issues } },
  }
  const { vm, unmount, props, events } = mountSharePanel(f)
  try {
    await settle()
    vm.selectedOrgId = 'org-1'
    vm.addPopupVisible = true

    await vm.handleShare()

    assert.equal(f.shareCalls.length, 1)
    assert.deepEqual(vm.refusedIssues, issues)
    assert.equal(vm.refusedIssueSummary, '启用知识库检索时需要 ReRank 模型')
    assert.equal(vm.canGoConfigure, true)
    assert.equal(vm.addPopupVisible, false, 'the add-share popup makes room for the notice')
    assert.equal(f.toasts.some(toast => toast.text.includes('Agent is not fully configured')), false,
      'the generic English message is replaced by the specific one')

    vm.goConfigure()
    assert.equal(events.length, 1)
    assert.equal(events[0].name, 'configure')
    assert.deepEqual(events[0].args, [issues])
    // The editor highlights the field the issue points at.
    assert.equal(agentReadinessHighlightField(issues[0]), 'rerank_model')
  } finally { unmount() }
})

test('every missing item is listed; a refusal without issues keeps its message', async () => {
  const f = fixture()
  const { vm, unmount, props } = mountSharePanel(f)
  try {
    await settle()
    vm.selectedOrgId = 'org-1'
    f.responses.share = {
      success: false,
      message: 'not used',
      error: { code: 1010, details: { issues: [
        { code: 'missing_chat_model', field: 'config.model_id' },
        { code: 'rerank_model_unavailable', field: 'config.rerank_model_id' },
        { code: 'rerank_model_unavailable', field: 'config.rerank_model_id' },
      ] } },
    }
    await vm.handleShare()
    assert.equal(vm.refusedIssueSummary, '缺少对话模型和所选模型已不可用')

    // Another agent, and a refusal that carries no issues. (The component
    // builds its own empty list, from another realm than this test's.)
    props.agentId = 'agent-2'
    await settle()
    assert.equal(vm.refusedIssues.length, 0, 'the notice does not follow the user to another agent')

    vm.selectedOrgId = 'org-1'
    f.responses.share = { success: false, message: 'Permission denied or invalid operation' }
    await vm.handleShare()
    assert.equal(vm.refusedIssues.length, 0)
    assert.deepEqual(f.toasts, [{ kind: 'error', text: 'Permission denied or invalid operation' }])
    assert.equal(vm.selectedOrgId, 'org-1')
  } finally { unmount() }
})
