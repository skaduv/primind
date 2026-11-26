import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

// Both pages that show starter suggestions must address the agent by
// "ID + source workspace". A shared agent is only reachable through its share,
// and two agents may share an ID across workspaces, so a request or a watcher
// that looks at the ID alone misses a switch between them (BF-04).
const pages = [
  { name: 'new conversation page', file: new URL('./creatChat/creatChat.vue', import.meta.url), store: 'settingsStore' },
  { name: 'empty conversation page', file: new URL('./chat/index.vue', import.meta.url), store: 'useSettingsStoreInstance' },
]

for (const page of pages) {
  // The working tree may be checked out with CRLF; match on LF only.
  const source = readFileSync(page.file, 'utf8').replace(/\r\n/g, '\n')
  const fetch = source.slice(
    source.indexOf('const fetchSuggestedQuestions = async'),
    source.indexOf('const handleSuggestedQuestionClick'),
  )
  const watchStart = source.indexOf('watch(\n    () => ({', source.indexOf('const debouncedFetch'))
  const watcher = watchStart < 0 ? '' : source.slice(watchStart, source.indexOf('deep: true', watchStart))

  test(`${page.name}: the suggestion request addresses the agent by ID and source workspace`, () => {
    assert.notEqual(fetch, '')
    assert.match(fetch, new RegExp(`const agent = ${page.store}\\.selectedAgentRef;`))
    assert.match(fetch, /getSuggestedQuestions\(agent, /)
    assert.doesNotMatch(fetch, new RegExp(`getSuggestedQuestions\\(\\s*${page.store}\\.selectedAgentId\\b`),
      'a bare agent ID cannot tell a shared agent from a same-ID local one')
    assert.match(fetch, /suggestedQuestionsErrorLabel\(err\)/, 'a failed request logs its error code')
  })

  test(`${page.name}: the suggestion watcher also watches the source workspace`, () => {
    assert.notEqual(watcher, '', 'suggestion watcher not found')
    assert.match(watcher, new RegExp(`agentId: ${page.store}\\.selectedAgentId,`))
    assert.match(watcher, new RegExp(`sourceTenantId: ${page.store}\\.selectedAgentSourceTenantId,`))
  })
}
