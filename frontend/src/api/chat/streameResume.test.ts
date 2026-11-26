// BF-03: a continue-stream handshake that answers 404 is not a connection
// failure — the backend has no stream for the message. The stream layer
// records why so the chat view can end the "replying" state quietly, while a
// 404 on a normal question stays an ordinary error.
import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { createServer, type ViteDevServer } from 'vite'
import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'

type Handshake = { ok: boolean, status: number, json?: () => Promise<unknown> }

type Call = {
  url: string
  options: {
    onopen: (res: Handshake) => Promise<void>
    onerror: (err: unknown) => never
  }
  resolve: () => void
  reject: (err: unknown) => void
}

let server: ViteDevServer
let useStream: typeof import('./streame').useStream
let transport: { calls: Call[] }
const originalStorage = Object.getOwnPropertyDescriptor(globalThis, 'localStorage')

before(async () => {
  Object.defineProperty(globalThis, 'localStorage', { configurable: true, value: {
    getItem: (key: string) => key === 'primind_token' ? 'test-token' : null,
  } })
  const mocks: Record<string, string> = {
    '@microsoft/fetch-event-source': `export const calls = [];
      export function fetchEventSource(url, options) {
        return new Promise((resolve, reject) => { calls.push({ url, options, resolve, reject }) })
      }`,
    '@/utils/index': `export const generateRandomString = () => 'test-request';`,
    '@/i18n': `export default { global: { t: key => key, locale: { value: 'zh-CN' } } };`,
  }
  server = await createServer({
    configFile: false,
    plugins: [{ name: 'stream-resume-test', enforce: 'pre',
      resolveId(id) { if (id.startsWith('\0mock:')) return id },
      load(id) { if (id.startsWith('\0mock:')) return mocks[id.slice(6)] },
    }],
    optimizeDeps: { noDiscovery: true, entries: [] },
    resolve: { alias: [
      ...Object.keys(mocks).map(find => ({ find, replacement: '\0mock:' + find })),
      { find: '@', replacement: fileURLToPath(new URL('../../', import.meta.url)) },
    ] },
    server: { middlewareMode: true, hmr: false, watch: null }, appType: 'custom',
  })
  ;({ useStream } = await server.ssrLoadModule('/src/api/chat/streame.ts'))
  transport = await server.ssrLoadModule('\0mock:@microsoft/fetch-event-source') as typeof transport
})
after(async () => {
  await server?.close()
  if (originalStorage) Object.defineProperty(globalThis, 'localStorage', originalStorage)
  else Reflect.deleteProperty(globalThis, 'localStorage')
})

/**
 * Starts a stream and answers its handshake like fetch-event-source would: a
 * rejected onopen is handed to onerror, and the error onerror throws is what
 * the fetch promise rejects with.
 */
async function handshake(params: Parameters<ReturnType<typeof useStream>['startStream']>[0], res: Handshake) {
  let stream!: ReturnType<typeof useStream>
  await renderToString(createSSRApp({ setup() { stream = useStream(); return () => null } }))
  const base = transport.calls.length
  const done = stream.startStream(params)
  for (let i = 0; i < 100 && transport.calls.length <= base; i++) await new Promise(r => setTimeout(r, 5))
  const call = transport.calls[transport.calls.length - 1]
  call.options.onopen(res).then(
    () => call.resolve(),
    (err: unknown) => {
      try {
        call.options.onerror(err)
        call.resolve()
      } catch (wrapped) {
        call.reject(wrapped)
      }
    },
  )
  await done
  return { stream, call }
}

const resume = { session_id: 's', query: 'a1', method: 'GET', url: '/api/v1/sessions/continue-stream' } as const
const question = { session_id: 's', query: 'hello', method: 'POST', url: '/api/v1/agent-chat' } as const
const notFound = (status?: string): Handshake => ({
  ok: false, status: 404,
  json: async () => (status ? { success: false, error: 'Incomplete message not found', status } : {}),
})

test('a resume 404 is recorded with the turn status the backend reported', async () => {
  const { stream, call } = await handshake(resume, notFound('interrupted'))
  assert.match(call.url, /continue-stream/)
  assert.deepEqual(stream.resumeFailure.value, { kind: 'not_found', status: 'interrupted' })
  // The generic error path still runs; the chat view decides to stay quiet.
  assert.match(String(stream.error.value), /HTTP 404/)
  assert.equal(stream.isStreaming.value, false)
})

test('a resume 404 without a body is still recognised', async () => {
  const { stream } = await handshake(resume, notFound())
  assert.deepEqual(stream.resumeFailure.value, { kind: 'not_found', status: '' })
})

test('a 404 on a normal question is an ordinary error', async () => {
  const { stream } = await handshake(question, notFound('failed'))
  assert.equal(stream.resumeFailure.value, null, 'only continue-stream 404s are quiet')
  assert.match(String(stream.error.value), /HTTP 404/)
})

test('other HTTP failures on a resume keep the existing handling', async () => {
  const { stream } = await handshake(resume, { ok: false, status: 500, json: async () => ({}) })
  assert.equal(stream.resumeFailure.value, null)
  assert.match(String(stream.error.value), /HTTP 500/)
})

test('a new stream clears the previous resume failure', async () => {
  const first = await handshake(resume, notFound('interrupted'))
  assert.ok(first.stream.resumeFailure.value)
  const next = await handshake(question, { ok: true, status: 200 })
  assert.equal(next.stream.resumeFailure.value, null)
  assert.equal(next.stream.error.value, null)
})
