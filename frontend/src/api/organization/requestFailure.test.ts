import assert from 'node:assert/strict'
import test from 'node:test'

import { requestFailure } from './requestFailure.ts'

const FALLBACK = 'Failed to preview organization'

// What utils/request rejects with for a backend error: the body is spread
// onto the object and the HTTP status is recorded twice (enumerable `status`
// and the non-enumerable `$httpStatus`).
function interceptorRejection(status: number, body: Record<string, unknown>) {
  const rejection = { status, message: undefined as string | undefined, ...body }
  Object.defineProperty(rejection, '$httpStatus', { value: status, enumerable: false })
  return rejection
}

test('a rejection carrying error.code keeps the code and the status', () => {
  const failure = requestFailure(
    interceptorRejection(404, {
      success: false,
      message: 'Invalid invite code',
      error: { code: 2401, message: 'Invalid invite code', details: null },
    }),
    FALLBACK,
  )
  assert.deepEqual(failure, { success: false, message: 'Invalid invite code', code: 2401, status: 404 })
})

test('the business code is optional and never guessed from the status', () => {
  const untyped = requestFailure(interceptorRejection(404, { message: 'Invalid invite code', error: { code: 1003 } }), FALLBACK)
  assert.deepEqual(untyped, { success: false, message: 'Invalid invite code', code: 1003, status: 404 })

  const noCode = requestFailure(interceptorRejection(500, { message: 'Internal server error' }), FALLBACK)
  assert.deepEqual(noCode, { success: false, message: 'Internal server error', status: 500 })

  const stringError = requestFailure(interceptorRejection(502, { message: 'Bad gateway', error: 'Bad gateway' }), FALLBACK)
  assert.deepEqual(stringError, { success: false, message: 'Bad gateway', status: 502 })
})

test('a rejection without an enumerable status falls back to the recorded HTTP status', () => {
  const rejection = { message: 'Failed to look up invite code' }
  Object.defineProperty(rejection, '$httpStatus', { value: 500, enumerable: false })
  assert.deepEqual(requestFailure(rejection, FALLBACK), { success: false, message: 'Failed to look up invite code', status: 500 })
})

test('a network error carries neither a code nor a status', () => {
  assert.deepEqual(requestFailure({ message: '网络错误，请检查网络连接' }, FALLBACK), {
    success: false,
    message: '网络错误，请检查网络连接',
  })
})

test('a missing message falls back to the caller default', () => {
  assert.deepEqual(requestFailure(undefined, FALLBACK), { success: false, message: FALLBACK })
  assert.deepEqual(requestFailure(null, FALLBACK), { success: false, message: FALLBACK })
  assert.deepEqual(requestFailure({ message: '' }, FALLBACK), { success: false, message: FALLBACK })
  assert.deepEqual(requestFailure(interceptorRejection(500, {}), FALLBACK), { success: false, message: FALLBACK, status: 500 })
})
