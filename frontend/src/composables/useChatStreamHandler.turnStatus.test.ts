import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test, { type TestContext } from 'node:test'
import vm from 'node:vm'
import ts from 'typescript'
import * as vue from 'vue'
import * as finalContent from '../utils/finalArtifactContent.ts'
import * as history from '../utils/rag-pipeline-history.ts'
import * as timestamps from '../utils/messageTimestamp.ts'
import * as steering from '../utils/steerStreamFork.ts'
import * as turnStatus from '../utils/messageTurnStatus.ts'
import type { useChatStreamHandler as StreamHandler } from './useChatStreamHandler.ts'

// BF-03 §6.2: how the chat handler treats a turn's terminal state. Loaded like
// the integration test: the whole production composable, transpiled, with only
// useI18n stubbed.
const source = readFileSync(new URL('./useChatStreamHandler.ts', import.meta.url), 'utf8')
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText
const modules: Record<string, unknown> = {
  vue, 'vue-i18n': { useI18n: () => ({ t: (key: string) => key }) },
  '@/utils/finalArtifactContent': finalContent,
  '@/utils/rag-pipeline-history': history,
  '@/utils/messageTimestamp': timestamps,
  '@/utils/steerStreamFork': steering,
  '@/utils/messageTurnStatus': turnStatus,
}
const exports: { useChatStreamHandler?: typeof StreamHandler } = {}
vm.runInNewContext(compiled, {
  exports, console,
  require: (id: string) => {
    assert.ok(id in modules, `unexpected import ${id}`)
    return modules[id]
  },
})
const useChatStreamHandler = exports.useChatStreamHandler!

function setup(t: TestContext, agentMode: boolean) {
  const messagesList = vue.reactive<Record<string, any>[]>([])
  const loading = vue.ref(true), isReplying = vue.ref(true)
  const currentAssistantMessageId = vue.ref(''), fullContent = vue.ref('')
  const errors: string[] = []
  const handler = useChatStreamHandler({
    messagesList, loading, isReplying, currentAssistantMessageId, fullContent,
    isAgentStreamSession: () => agentMode, scrollToBottom() {},
    onError: error => errors.push(error),
  })
  const assistant = () => {
    const message = vue.reactive<Record<string, any>>({
      id: 'assistant', assistant_message_id: 'assistant', request_id: 'request', role: 'assistant',
      content: '', is_completed: false, isAgentMode: agentMode, agentEventStream: [], _eventMap: new Map(),
    })
    messagesList.push(message)
    currentAssistantMessageId.value = 'assistant'
    return message
  }
  return { handler, messagesList, loading, isReplying, currentAssistantMessageId, errors, assistant }
}

test('a failed turn in history is not treated as still generating', t => {
  const { handler, messagesList } = setup(t, true)
  void handler.handleMsgList([
    { id: 'u', role: 'user', content: 'q', is_completed: true },
    { id: 'a', role: 'assistant', content: '', is_completed: true, status: 'failed',
      error_code: 'kb_search_failed', error_message: '知识库检索失败' },
  ])
  const failed = messagesList[1]
  assert.equal(turnStatus.isGeneratingTurn(failed), false)
  assert.equal(turnStatus.isFailedOrInterruptedTurn(failed), true, 'shows the failure and the retry button')
  assert.equal(handler.findLastMessage(m => m.role === 'assistant' && turnStatus.isGeneratingTurn(m)), undefined,
    'nothing to resume')
})

test('an interrupted turn in history is not resumed either', t => {
  const { handler, messagesList } = setup(t, false)
  void handler.handleMsgList([
    { id: 'a', role: 'assistant', content: 'partial', is_completed: true, status: 'interrupted',
      error_code: 'process_lost', error_message: '本次回答已中断，可以重新提问' },
  ])
  assert.equal(turnStatus.isGeneratingTurn(messagesList[0]), false)
  assert.equal(handler.findLastMessage(m => turnStatus.isGeneratingTurn(m)), undefined)
})

test('a legacy row with status unset and is_completed=false still resumes (old backend)', t => {
  const { handler, messagesList } = setup(t, true)
  void handler.handleMsgList([{ id: 'a', role: 'assistant', content: '', is_completed: false }])
  assert.equal(turnStatus.isGeneratingTurn(messagesList[0]), true)
  assert.equal(handler.findLastMessage(m => m.role === 'assistant' && turnStatus.isGeneratingTurn(m)), messagesList[0])
})

test('a legacy completed row with status unset is terminal', t => {
  const { handler, messagesList } = setup(t, false)
  void handler.handleMsgList([{ id: 'a', role: 'assistant', content: 'done', is_completed: true }])
  assert.equal(turnStatus.effectiveTurnStatus(messagesList[0]), 'completed')
  assert.equal(turnStatus.isGeneratingTurn(messagesList[0]), false)
})

test('the turn_interrupted event of continue-stream ends replying quietly', t => {
  for (const agentMode of [true, false]) {
    const h = setup(t, agentMode)
    const message = h.assistant()
    h.handler.processStreamChunk({
      id: 'request', response_type: 'error', content: '本次回答已中断，可以重新提问', done: true,
      data: { code: 'turn_interrupted' },
    })
    assert.equal(message.is_completed, true)
    assert.equal(message.status, 'interrupted')
    assert.equal(message.error_code, 'process_lost')
    assert.equal(message.error_message, '本次回答已中断，可以重新提问')
    assert.equal(turnStatus.isFailedOrInterruptedTurn(message), true, 'offers to ask again')
    assert.equal(h.isReplying.value, false, '"replying" ends')
    assert.equal(h.loading.value, false)
    assert.equal(h.currentAssistantMessageId.value, '')
    assert.deepEqual(h.errors, [], 'no global error toast')
    assert.equal(message.content, '', 'the explanation is not written into the answer body')
  }
})

test('markTurnInterrupted fills a message the resume 404 left behind', t => {
  const { handler, messagesList } = setup(t, false)
  const message = h_assistant(messagesList)
  handler.markTurnInterrupted(message)
  assert.equal(message.status, 'interrupted')
  assert.equal(message.is_completed, true)
  assert.equal(message.error_code, 'process_lost')
  handler.markTurnInterrupted(undefined)
})

function h_assistant(messagesList: Record<string, any>[]) {
  const message = vue.reactive<Record<string, any>>({
    id: 'assistant', role: 'assistant', content: '', is_completed: false,
  })
  messagesList.push(message)
  return message
}

test('a live stream error marks the turn failed with the backend code', t => {
  const h = setup(t, true)
  const message = h.assistant()
  h.handler.processStreamChunk({
    id: 'request', response_type: 'error', content: 'model exploded', done: true,
    data: { code: 'model_error', stage: 'agent_execution' },
  })
  assert.equal(message.status, 'failed')
  assert.equal(message.error_code, 'model_error')
  assert.equal(message.is_completed, true)
  assert.deepEqual(h.errors, ['model exploded'], 'a live failure is still reported')
  assert.equal(h.isReplying.value, false)

  // The engine announces completion after an error: the failure stays.
  h.handler.processStreamChunk({ id: 'request', response_type: 'complete', done: true, data: {} })
  assert.equal(message.status, 'failed')
  assert.equal(message.error_code, 'model_error')
})

test('a persistence failure records persist_failed', t => {
  const h = setup(t, true)
  const message = h.assistant()
  h.handler.processStreamChunk({
    id: 'request', response_type: 'error', content: 'Failed to save assistant message', done: true,
    data: { stage: 'message_persistence', code: 'persist_failed' },
  })
  assert.equal(message.persistence_error, 'Failed to save assistant message')
  assert.equal(message.status, 'failed')
  assert.equal(message.error_code, 'persist_failed')
})

test('a stop records stopped, not failed', t => {
  const h = setup(t, true)
  const message = h.assistant()
  h.handler.processStreamChunk({ id: 'request', response_type: 'stop', done: true, data: { reason: 'user_requested' } })
  assert.equal(message.status, 'stopped')
  assert.equal(message.is_completed, true)
  assert.equal(turnStatus.isFailedOrInterruptedTurn(message), false, 'a stop offers no retry')
})

test('a normal completion records completed', t => {
  const h = setup(t, false)
  const message = h.assistant()
  h.handler.processStreamChunk({ id: 'request', response_type: 'answer', content: 'hi', done: false })
  h.handler.processStreamChunk({ id: 'request', response_type: 'complete', done: true, data: {} })
  assert.equal(message.is_completed, true)
  assert.equal(turnStatus.effectiveTurnStatus(message), 'completed')
})

test('an agent error keeps what was already generated', t => {
  const h = setup(t, true)
  const message = h.assistant()
  message.content = 'partial answer'
  h.handler.processStreamChunk({
    id: 'request', response_type: 'error', content: 'model exploded', done: true,
    data: { code: 'agent_error', stage: 'agent_execution' },
  })
  assert.equal(message.content, 'partial answer', 'the error text does not replace the answer')
  assert.equal(message.error_message, 'model exploded')
  assert.equal(message.status, 'failed')
  assert.equal(turnStatus.isFailedOrInterruptedTurn(message), true)
})

test('a quick-answer stream error ends the turn as failed, not completed', t => {
  const h = setup(t, false)
  const message = h.assistant()
  h.handler.processStreamChunk({ id: 'request', response_type: 'answer', content: 'half', done: false })
  h.handler.processStreamChunk({
    id: 'request', response_type: 'error', content: 'model stream broke', done: true,
    data: { code: 'model_error', stage: 'chat_completion_stream' },
  })
  const row = h.messagesList.find(m => m.role === 'assistant')!
  assert.equal(row, message)
  assert.equal(turnStatus.effectiveTurnStatus(row), 'failed')
  assert.equal(row.error_code, 'model_error')
  assert.equal(row.error_message, 'model stream broke')
  assert.equal(row.content, 'half', 'the partial answer stays')
  assert.equal(row.is_completed, true)
  assert.equal(h.isReplying.value, false)
  assert.deepEqual(h.errors, ['model stream broke'])
})
