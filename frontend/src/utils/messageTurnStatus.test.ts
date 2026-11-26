import assert from 'node:assert/strict'
import test from 'node:test'
import {
  TURN_INTERRUPTED_SSE_CODE,
  applyTurnOutcome,
  effectiveTurnStatus,
  isFailedOrInterruptedTurn,
  isGeneratingTurn,
  isTurnInterruptedEvent,
  turnErrorCode,
  turnErrorMessage,
} from './messageTurnStatus.ts'

test('an explicit status always wins', () => {
  assert.equal(effectiveTurnStatus({ role: 'assistant', status: 'failed', is_completed: true }), 'failed')
  assert.equal(effectiveTurnStatus({ role: 'assistant', status: 'interrupted', is_completed: true }), 'interrupted')
  assert.equal(effectiveTurnStatus({ role: 'assistant', status: 'stopped', is_completed: true }), 'stopped')
  assert.equal(effectiveTurnStatus({ role: 'assistant', status: 'generating', is_completed: false }), 'generating')
})

test('rows from before the status column are read through is_completed', () => {
  assert.equal(effectiveTurnStatus({ role: 'assistant', is_completed: true }), 'completed')
  assert.equal(effectiveTurnStatus({ role: 'assistant', is_completed: false }), 'generating')
  assert.equal(effectiveTurnStatus({ role: 'assistant', status: '', is_completed: false }), 'generating')
  assert.equal(effectiveTurnStatus(undefined), 'completed')
})

test('only a generating assistant turn resumes its stream', () => {
  assert.equal(isGeneratingTurn({ role: 'assistant', is_completed: false }), true, 'old backend, still running')
  assert.equal(isGeneratingTurn({ role: 'assistant', status: 'generating' }), true)
  assert.equal(isGeneratingTurn({ role: 'assistant', status: 'failed', is_completed: true }), false)
  assert.equal(isGeneratingTurn({ role: 'assistant', status: 'interrupted', is_completed: true }), false)
  assert.equal(isGeneratingTurn({ role: 'assistant', is_completed: true }), false)
  assert.equal(isGeneratingTurn({ role: 'user', is_completed: false }), false)
  assert.equal(isGeneratingTurn(undefined), false)
})

test('failed and interrupted turns offer to ask again; stops do not', () => {
  assert.equal(isFailedOrInterruptedTurn({ role: 'assistant', status: 'failed', is_completed: true }), true)
  assert.equal(isFailedOrInterruptedTurn({ role: 'assistant', status: 'interrupted', is_completed: true }), true)
  assert.equal(isFailedOrInterruptedTurn({ role: 'assistant', status: 'stopped', is_completed: true }), false)
  assert.equal(isFailedOrInterruptedTurn({ role: 'assistant', is_completed: true }), false)
  assert.equal(isFailedOrInterruptedTurn({ role: 'user', status: 'failed' }), false)
})

test('recognizes the quiet interrupted event of continue-stream', () => {
  assert.equal(isTurnInterruptedEvent({
    response_type: 'error', done: true, data: { code: TURN_INTERRUPTED_SSE_CODE },
  }), true)
  assert.equal(isTurnInterruptedEvent({
    response_type: 'error', done: true, data: { stage: 'knowledge_qa_execution' },
  }), false)
  assert.equal(isTurnInterruptedEvent({ response_type: 'answer', data: { code: TURN_INTERRUPTED_SSE_CODE } }), false)
  assert.equal(isTurnInterruptedEvent(undefined), false)
})

test('applies a reported outcome to the message', () => {
  const message: Record<string, unknown> = {
    role: 'assistant', is_completed: false, thinking: true, artifactsCollecting: true,
  }
  applyTurnOutcome(message, { status: 'interrupted', error_code: 'process_lost', error_message: 'interrupted' })
  assert.equal(message.is_completed, true)
  assert.equal(message.status, 'interrupted')
  assert.equal(message.error_code, 'process_lost')
  assert.equal(message.error_message, 'interrupted')
  assert.equal(message.thinking, false)
  assert.equal(message.artifactsCollecting, false)
  assert.equal(isFailedOrInterruptedTurn(message as never), true)
  assert.equal(isGeneratingTurn(message as never), false)
  applyTurnOutcome(message, { status: 'interrupted' })
  assert.equal(message.error_code, '')
  assert.equal(message.error_message, '')
})

test('reads the stored reason defensively', () => {
  assert.equal(turnErrorCode({ error_code: 'model_error' }), 'model_error')
  assert.equal(turnErrorCode({ error_code: 42 }), '')
  assert.equal(turnErrorCode(undefined), '')
  assert.equal(turnErrorMessage({ error_message: 'broke' }), 'broke')
  assert.equal(turnErrorMessage({}), '')
})
