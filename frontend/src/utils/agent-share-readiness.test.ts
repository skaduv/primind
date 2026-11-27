import assert from 'node:assert/strict'
import test from 'node:test'
import {
  agentReadinessHighlightField,
  agentReadinessIssueLabels,
  firstReadinessHighlightField,
  isSharedAgentNotReady,
  parseAgentReadinessIssues,
  parseAgentSaveWarnings,
  shareRefusalIssues,
  sharedAgentNotReadyFields,
  sharedAgentNotReadyIssues,
} from './agent-share-readiness.ts'

const missingRerank = { code: 'missing_rerank_model', field: 'config.rerank_model_id' }

test('a refused share keeps the issues the server sent', () => {
  assert.deepEqual(
    shareRefusalIssues({
      success: false,
      message: 'Agent is not fully configured. …',
      error: { code: 1010, message: 'Agent is not fully configured. …', details: { issues: [missingRerank] } },
    }),
    [missingRerank],
  )
})

test('a refused share without issues, or with a malformed body, reports none', () => {
  assert.deepEqual(shareRefusalIssues({ success: false, message: 'Permission denied' }), [])
  assert.deepEqual(shareRefusalIssues({ error: { code: 1002, details: null } }), [])
  assert.deepEqual(shareRefusalIssues({ error: { code: 1010, details: 'not an object' } }), [])
  assert.deepEqual(shareRefusalIssues(null), [])
})

test('issue parsing drops malformed entries and tolerates a missing field', () => {
  assert.deepEqual(
    parseAgentReadinessIssues([{ code: 'missing_chat_model' }, 'x', { field: 'config.model_id' }, { code: '', field: 'x' }]),
    [{ code: 'missing_chat_model', field: '' }],
  )
  assert.deepEqual(parseAgentReadinessIssues(undefined), [])
})

test('an issue points at the editor field that fixes it', () => {
  assert.equal(agentReadinessHighlightField({ code: 'missing_chat_model', field: 'config.model_id' }), 'summary_model')
  assert.equal(agentReadinessHighlightField(missingRerank), 'rerank_model')
  assert.equal(agentReadinessHighlightField({ code: 'something_new', field: 'config.other' }), null)
  assert.equal(agentReadinessHighlightField({ code: 'missing_chat_model', field: '' }), null)
})

test('the first issue with a known field decides where to go', () => {
  assert.equal(
    firstReadinessHighlightField([{ code: 'x', field: 'config.other' }, { code: 'y', field: 'config.model_id' }, missingRerank]),
    'summary_model',
  )
  assert.equal(firstReadinessHighlightField([]), null)
})

test('labels are translated once each, and unknown codes are skipped', () => {
  const t = (key: string) => key.split('.').pop() ?? key
  assert.deepEqual(
    agentReadinessIssueLabels(t, [
      missingRerank,
      { code: 'chat_model_unavailable', field: 'config.model_id' },
      { code: 'rerank_model_unavailable', field: 'config.rerank_model_id' },
      { code: 'rerank_model_unavailable', field: 'config.rerank_model_id' },
      { code: 'new_kind', field: 'config.model_id' },
    ]),
    ['issueMissingRerankModel', 'issueChatModelUnavailable', 'issueRerankModelUnavailable'],
  )
})

test('a save response carries its warnings, and only the shared-agent one matters here', () => {
  const warnings = parseAgentSaveWarnings({
    success: true,
    data: { id: 'agent-1' },
    warnings: [{ code: 'something_else' }, { code: 'shared_agent_not_ready', issues: [missingRerank] }, 'junk'],
  })
  assert.deepEqual(warnings, [
    { code: 'something_else', issues: [] },
    { code: 'shared_agent_not_ready', issues: [missingRerank] },
  ])
  assert.deepEqual(sharedAgentNotReadyIssues(warnings), [missingRerank])
  assert.deepEqual(parseAgentSaveWarnings({ success: true, data: {} }), [])
  assert.deepEqual(sharedAgentNotReadyIssues([{ code: 'shared_agent_not_ready', issues: [] }]), [])
})

test('only an explicit ready=false marks a shared agent as not ready', () => {
  assert.equal(isSharedAgentNotReady({ ready: false }), true)
  assert.equal(isSharedAgentNotReady({ ready: true }), false)
  assert.equal(isSharedAgentNotReady({}), false, 'an older backend sends no verdict')
  assert.equal(isSharedAgentNotReady(undefined), false)
})

// The send and select checks in the chat input block a shared agent on the
// server's verdict, and point at the fields the server named.
test('the fields behind a not-ready shared agent', () => {
  const missingChat = { code: 'missing_chat_model', field: 'config.model_id' }
  assert.deepEqual(sharedAgentNotReadyFields({ ready: false, issues: [missingRerank] }), ['rerank_model'])
  assert.deepEqual(
    sharedAgentNotReadyFields({
      ready: false,
      issues: [missingChat, missingRerank, { code: 'rerank_model_unavailable', field: 'config.rerank_model_id' }],
    }),
    ['summary_model', 'rerank_model'],
    'one field per config path, in order',
  )
  assert.deepEqual(
    sharedAgentNotReadyFields({ ready: false, issues: [{ code: 'future_code', field: 'config.other' }] }),
    [],
    'not ready even when no field can be named',
  )
  assert.deepEqual(sharedAgentNotReadyFields({ ready: false }), [])
  assert.equal(sharedAgentNotReadyFields({ ready: true, issues: [missingRerank] }), null)
  assert.equal(sharedAgentNotReadyFields({}), null, 'an older backend sends no verdict')
  assert.equal(sharedAgentNotReadyFields(undefined), null)
})
