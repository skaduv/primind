// BF-03 §4.7: the chat view must not turn an ended turn into a stuck
// "preparing an answer…" plus a "stream failed" toast on every open.
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const source = readFileSync(new URL('./index.vue', import.meta.url), 'utf8')
const stream = readFileSync(new URL('../../api/chat/streame.ts', import.meta.url), 'utf8')
const notice = readFileSync(new URL('./components/TurnStatusNotice.vue', import.meta.url), 'utf8')

function slice(startMarker, endMarker) {
  const start = source.indexOf(startMarker)
  assert.notEqual(start, -1, startMarker)
  const end = source.indexOf(endMarker, start)
  assert.notEqual(end, -1, endMarker)
  return source.slice(start, end)
}

test('a failed or interrupted assistant turn shows its state and a retry button', () => {
  const template = slice('<div v-if="session.role == \'assistant\' && (shouldRenderAssistantMessage', '<FollowUpSuggestions')
  // The bubble is hidden for an answer with nothing to render, but a failed or
  // interrupted turn must still show its state.
  assert.match(template, /shouldRenderAssistantMessage\(session\) \|\| isFailedOrInterruptedTurn\(session\)/)
  assert.match(template, /<botmsg v-if="shouldRenderAssistantMessage\(session\)"/)
  assert.match(template, /<TurnStatusNotice v-if="isFailedOrInterruptedTurn\(session\)" :session="session"/)
  assert.match(template, /@retry="handleRetryTurn\(session, index\)"/)
  assert.match(source, /import TurnStatusNotice from '\.\/components\/TurnStatusNotice\.vue'/)
  assert.match(notice, /chat\.turnStatus\.\$\{status\}/)
  assert.match(notice, /chat\.turnError\.\$\{code\}/)
  assert.match(notice, /turnErrorMessage\(props\.session\)/, 'falls back to the explanation the backend stored')
})

test('a resume 404 ends "replying" quietly instead of toasting "stream failed"', () => {
  const watcher = slice('watch(error, (newError) => {', 'onChunk((data) => {')
  const im = watcher.indexOf('if (isAttachingImStream.value)')
  const resume = watcher.indexOf("if (resumeFailure.value?.kind === 'not_found')")
  const toast = watcher.indexOf('MessagePlugin.error(newError)')
  assert.ok(im >= 0 && resume > im && toast > resume, 'IM recovery first, quiet resume second, toast last')
  const quiet = watcher.slice(resume, toast)
  assert.match(quiet, /markTurnInterrupted\(target\)/)
  assert.match(quiet, /isReplying\.value = false/)
  assert.match(quiet, /loading\.value = false/)
  assert.doesNotMatch(quiet, /MessagePlugin\./, 'no error toast for a turn that simply ended')
})

test('a resume 404 acts on the turn status the backend reported', () => {
  const watcher = slice('watch(error, (newError) => {', 'onChunk((data) => {')
  const resume = watcher.indexOf("if (resumeFailure.value?.kind === 'not_found')")
  const quiet = watcher.slice(resume, watcher.indexOf('MessagePlugin.error(newError)'))
  assert.match(quiet, /resumeNotFoundAction\(resumeFailure\.value\.status\)/)
  // Still generating on the server: keep "replying" and retry, before anything is cleared.
  const retry = quiet.indexOf("if (action === 'retry' && resumeId && scheduleResumeRetry(resumeId)) return;")
  assert.ok(retry >= 0 && retry < quiet.indexOf('isReplying.value = false'))
  // Ended on the server meanwhile: reload what was saved, never mark it interrupted.
  const reload = quiet.indexOf("if (action === 'reload')")
  assert.ok(reload >= 0 && reload < quiet.indexOf('markTurnInterrupted(target)'))
  assert.match(quiet.slice(reload), /reloadHistoryAfterResume\(\);\s*return;/)
})

test('resume retries are bounded and cancelled with the turn', () => {
  const schedule = slice('const scheduleResumeRetry = (resumeId) => {', 'const reloadHistoryAfterResume')
  assert.match(schedule, /resumeRetry\.attempts > RESUME_RETRY_MAX_ATTEMPTS\) return false/)
  assert.match(schedule, /session_id\.value !== targetSession \|\| !isReplying\.value/)
  assert.match(schedule, /currentAssistantMessageId\.value !== resumeId\) return/)
  assert.match(schedule, /url: '\/api\/v1\/sessions\/continue-stream'/)
  const stop = slice('const handleStopGeneration = () => {', 'markInFlightAssistantStopped')
  assert.match(stop, /clearResumeRetry\(\)/, 'stopping cancels a pending resume')
  const unmount = slice('onUnmounted(() => {', 'onBeforeRouteLeave')
  assert.match(unmount, /clearResumeRetry\(\)/)
})

test('"ask again" resends the question of the failed turn with its mentions', () => {
  const retry = slice('const handleRetryTurn = (session, index) => {', 'watch([() => route.params]')
  assert.match(retry, /row\.request_id === session\.request_id/)
  assert.match(retry, /sendMsg\(question\.content, '', question\.mentioned_items \|\| \[\], \[\], \[\], \{\}\)/)
  assert.match(retry, /if \(composerLocked\.value \|\| isReplying\.value\) return/)
})

test('the stream layer only reports a quiet resume failure for continue-stream', () => {
  assert.match(stream, /const resuming = params\.method === 'GET' && \/continue-stream\/\.test\(params\.url\)/)
  assert.match(stream, /if \(resuming && res\.status === 404\)/)
  assert.match(stream, /resumeFailure\.value = \{ kind: 'not_found', status: await readResumeStatus\(res\) \}/)
  assert.match(stream, /resumeFailure\.value = null;/, 'reset on every start')
  assert.match(source, /lastStreamRequest, resumeFailure \} = useStream\(\)/)
})

test('history with a failed turn never starts a resume for it', () => {
  const hook = slice('onAfterMsgList: async () =>', 'onAgentQuery:')
  assert.match(hook, /isGeneratingTurn\(message\)/)
  assert.doesNotMatch(hook, /!message\.is_completed/)
})

test('a backend that keeps reporting generating past every retry is not declared interrupted', () => {
  const watcher = slice('watch(error, (newError) => {', 'onChunk((data) => {')
  const quiet = watcher.slice(watcher.indexOf("if (resumeFailure.value?.kind === 'not_found')"))
  const exhausted = quiet.indexOf("if (action === 'retry') return;")
  assert.ok(exhausted >= 0 && exhausted < quiet.indexOf('markTurnInterrupted(target)'),
    'only an old backend without a status gets the interrupted guess')
})

test('a failed or interrupted turn with nothing else to render keeps its row', () => {
  // The row wrapper hides assistant segments with nothing to render; the
  // status notice lives inside it and must stay visible.
  assert.match(source,
    /'is-empty-segment': session\.role === 'assistant' && !shouldRenderAssistantMessage\(session\) && !isFailedOrInterruptedTurn\(session\)/)
  const template = slice('<TurnStatusNotice v-if="isFailedOrInterruptedTurn(session)"', '@retry=')
  assert.match(template, /:embeddedMode="embeddedMode"/, 'the embedded chat styles the notice too')
})
