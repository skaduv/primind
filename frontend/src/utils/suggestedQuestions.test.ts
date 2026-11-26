import assert from 'node:assert/strict'
import test from 'node:test'

import { suggestedQuestionsErrorLabel } from './suggestedQuestions.ts'

// The request client rejects with the HTTP status and the backend error body.
test('names the HTTP status and the backend code of a failed suggestion request', () => {
  assert.equal(
    suggestedQuestionsErrorLabel({ status: 404, error: { code: 1003, message: 'Agent not found' } }),
    'HTTP 404, code 1003',
  )
  assert.equal(suggestedQuestionsErrorLabel({ status: 403 }), 'HTTP 403')
  assert.equal(suggestedQuestionsErrorLabel({ error: { code: 2001 } }), 'code 2001')
  assert.equal(suggestedQuestionsErrorLabel({ $httpStatus: 500, message: 'boom' }), 'HTTP 500')
})

test('falls back to the message when there is no code to report', () => {
  assert.equal(suggestedQuestionsErrorLabel({ message: 'Network Error' }), 'Network Error')
  assert.equal(suggestedQuestionsErrorLabel(undefined), 'unknown error')
  assert.equal(suggestedQuestionsErrorLabel(null), 'unknown error')
  assert.equal(suggestedQuestionsErrorLabel('raw'), 'unknown error')
})
