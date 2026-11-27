import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import { sharedAgentNotReadyFields } from '../utils/agent-share-readiness.ts'

// A receiver may have picked a shared agent before its creator broke the saved
// configuration. The chat input checks the agent again when a message is sent
// and when an agent is picked; for a shared agent the server's verdict
// (ready=false) decides, since the local rules do not cover all of it (e.g. a
// quick-answer agent that runs knowledge search needs a rerank model too).
const source = readFileSync(new URL('./Input-field.vue', import.meta.url), 'utf8')

const labels = {
  summary_model: '对话模型',
  rerank_model: '重排模型',
  allowed_tools: '允许的工具',
}

function loadCheck({ sharedAgents, localKeys = [] }) {
  const start = source.indexOf('const findSharedAgentInfo =')
  const end = source.indexOf('const goToAgentEditor =', start)
  assert.ok(start >= 0 && end > start, 'expected the readiness check')
  const { collectAgentNotReadyReasons } = vm.runInNewContext(
    ts.transpile(`${source.slice(start, end)}\n({ collectAgentNotReadyReasons })`),
    {
      orgStore: { sharedAgents },
      allModels: { value: [] },
      getAgentNotReadyReasonKeys: () => localKeys,
      formatAgentNotReadyReasons: (keys) => keys.map((key) => labels[key]),
      sharedAgentNotReadyFields,
      t: (key) => key,
    },
  )
  // Values built inside the sandbox have its prototypes; compare plain copies.
  return (...args) => {
    const result = collectAgentNotReadyReasons(...args)
    return { keys: Array.from(result.keys), labels: Array.from(result.labels) }
  }
}

const quickAnswer = { id: 'agent-1', name: '客服', is_builtin: false, config: { agent_mode: 'quick-answer' } }
const sharedInfo = (extra) => ({ agent: { id: 'agent-1' }, source_tenant_id: 7, ...extra })

test('a shared agent the server marks not ready is blocked, naming the fields', () => {
  const check = loadCheck({
    sharedAgents: [sharedInfo({
      ready: false,
      issues: [{ code: 'missing_rerank_model', field: 'config.rerank_model_id' }],
    })],
  })
  assert.deepEqual(check(quickAnswer, false, '7'), { keys: ['rerank_model'], labels: ['重排模型'] })
})

test('server and local findings are merged without repeats', () => {
  const check = loadCheck({
    localKeys: ['summary_model'],
    sharedAgents: [sharedInfo({
      ready: false,
      issues: [
        { code: 'missing_chat_model', field: 'config.model_id' },
        { code: 'missing_rerank_model', field: 'config.rerank_model_id' },
      ],
    })],
  })
  assert.deepEqual(check(quickAnswer, false, '7').keys, ['summary_model', 'rerank_model'])
})

test('a verdict that names no known field still blocks', () => {
  const check = loadCheck({
    sharedAgents: [sharedInfo({ ready: false, issues: [{ code: 'future_code', field: 'config.other' }] })],
  })
  assert.deepEqual(check(quickAnswer, false, '7'), {
    keys: [],
    labels: ['organization.share.sharedNotReady'],
  })
})

test('without a not-ready verdict only the local rules apply', () => {
  const ready = loadCheck({ sharedAgents: [sharedInfo({ ready: true })] })
  assert.deepEqual(ready(quickAnswer, false, '7'), { keys: [], labels: [] })

  const olderBackend = loadCheck({ sharedAgents: [sharedInfo({})] })
  assert.deepEqual(olderBackend(quickAnswer, false, '7'), { keys: [], labels: [] })

  // The verdict belongs to the agent shared from that workspace only, and an
  // own agent (no source workspace) is never looked up.
  const notReady = loadCheck({ sharedAgents: [sharedInfo({ ready: false, issues: [] })] })
  assert.deepEqual(notReady(quickAnswer, false, '8'), { keys: [], labels: [] })
  assert.deepEqual(notReady(quickAnswer, false, undefined), { keys: [], labels: [] })
})

test('both the send and the pick paths go through the check with the source workspace', () => {
  const send = source.slice(source.indexOf('// 发送前校验当前选中的智能体'), source.indexOf('// 获取@提及的知识库和文件信息'))
  assert.match(
    send,
    /collectAgentNotReadyReasons\(\s*actualAgent,\s*isAgentMode,\s*settingsStore\.selectedAgentSourceTenantId \?\? undefined,\s*\)/,
  )
  assert.match(send, /if \(notReadyReasons\.length > 0\) \{[\s\S]*?return;/)

  const pick = source.slice(source.indexOf('const handleSelectAgent = async'), source.indexOf('const clearvalue'))
  assert.match(pick, /collectAgentNotReadyReasons\(\s*actualAgent,\s*isAgentType,\s*sourceTenantId,\s*\)/)
})
