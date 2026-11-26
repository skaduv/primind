import assert from 'node:assert/strict'
import test from 'node:test'

import { createChatMarkdownRenderer, renderChatMarkdown } from './chatMarkdownRenderer.ts'
import {
  RESIDUAL_REF_RE,
  stripCitationTagsForMarkdown,
  stripIncompleteCitationTag,
  stripResidualRefMarkers,
} from './citationMarkdown.ts'
import { buildSessionMarkdown } from './sessionMarkdown.ts'

const options = {
  renderer: createChatMarkdownRenderer(),
  escapeMarkdown: (text: string) => text,
  sanitizeHtml: (html: string) => html,
}

/** Every bracket spelling the backend rule table accepts, plus the canonical angle form. */
const RESIDUAL_MARKERS = [
  '<ref id="c2"/>',
  '<ref id="c2">',
  '[ref id="c2"]',
  "[ref id='c2']",
  '[ref id=c2]',
  '[ref id=“c2”/]',
  '[ref id="c2]',
  '【ref id="c2"】',
  "【ref id='c2'】",
  '【ref id=c2】',
  '【ref id="c2]',
  '[ ref id = "c2" / ]',
  '[REF ID="C2"]',
]

/** Text that merely looks bracketed and must survive untouched (spec C2). */
const ORDINARY_BRACKETS = [
  'see [1] and [2]',
  '[文字](https://example.com)',
  '【注】仅供参考',
  'array[i] = a[j]',
  '[ref](https://example.com)',
  '[reference] and [ref id="note"]',
  '[ref id="c0"] [ref id=c01] [refid="c2"]',
  'a < b and 3 > 2',
  '<refnote id="c2"/>',
]

test('stripResidualRefMarkers removes every residual citation marker', () => {
  for (const marker of RESIDUAL_MARKERS) {
    assert.equal(stripResidualRefMarkers(`前文${marker}后文`), '前文后文', marker)
    assert.equal(RESIDUAL_REF_RE.lastIndex, 0, 'global regex must be reusable')
  }
  // The defect itself: three markers in one stored answer.
  assert.equal(
    stripResidualRefMarkers('交通法规 [ref id="c15"] [ref id="c22"]。'),
    '交通法规  。',
  )
})

test('stripResidualRefMarkers leaves ordinary brackets and plain text byte-identical', () => {
  for (const text of ORDINARY_BRACKETS) {
    assert.equal(stripResidualRefMarkers(text), text, text)
  }
  for (const text of ['', '# 标题\n\n- 列表 **粗体**\n\n```js\na[0]\n```\n', 'ref id = c2']) {
    assert.equal(stripResidualRefMarkers(text), text)
  }
})

test('stripIncompleteCitationTag hides an unfinished bracket citation tail while streaming', () => {
  const prefix = '有效期内'
  for (const partial of ['[ref', '[ref ', '[ref i', '[ref id="c', '【ref id="c1"', '[ ref id="c']) {
    assert.equal(stripIncompleteCitationTag(prefix + partial), prefix, partial)
  }
  // Once the bracket closes the marker is complete: it is no longer a tail,
  // and stripResidualRefMarkers (which runs first) has removed it.
  const complete = '[ref id="c2"]'
  assert.equal(stripIncompleteCitationTag(prefix + complete), prefix + complete)
  assert.equal(stripResidualRefMarkers(prefix + complete), prefix)
})

test('stripIncompleteCitationTag keeps ordinary trailing brackets', () => {
  for (const tail of ['[', '[r', '[re', '[refer', '[1]', '[ref](https://example.com)', '【注】', 'array[i', ' < 5']) {
    assert.equal(stripIncompleteCitationTag('值 ' + tail), '值 ' + tail, tail)
  }
  // Existing angle-bracket behaviour is unchanged.
  assert.equal(stripIncompleteCitationTag('Source <kb doc="a"'), 'Source ')
  assert.equal(stripIncompleteCitationTag('Source <kb doc="a" />'), 'Source <kb doc="a" />')
})

test('stripIncompleteCitationTag hides whichever unfinished tail comes first', () => {
  assert.equal(stripIncompleteCitationTag('a <kb doc="x" [ref id="c'), 'a ')
  assert.equal(stripIncompleteCitationTag('a [ref id="c1"] <kb doc="x'), 'a [ref id="c1"] ')
  assert.equal(stripIncompleteCitationTag('a [ref id="c1"] b < 5'), 'a [ref id="c1"] b < 5')
  assert.equal(stripIncompleteCitationTag('a [ref id="c1"] b [ref id="c2"'), 'a [ref id="c1"] b ')
})

test('renderChatMarkdown drops residual markers from stored history (AC2 / AC5)', () => {
  const history = [
    '……且交强险在有效期内[ref id="c2"]。',
    '……购买交强险和商业保险且在有效期内[ref id="c9"]。',
    '……并要求驾驶员严格遵守交通法规 [ref id="c15"] [ref id="c22"]。',
    '旧格式【ref id="c4"】和尖括号<ref id="c16"/>也要去掉。',
  ].join('\n\n')
  for (const streaming of [false, true]) {
    const html = renderChatMarkdown(history, { ...options, streaming })
    assert.doesNotMatch(html, /ref id=/)
    assert.doesNotMatch(html, /citation-kb/, 'no card can be rebuilt from a lost mapping')
    assert.match(html, /且交强险在有效期内。/)
  }
})

test('renderChatMarkdown keeps ordinary brackets (AC3)', () => {
  const text = '见 [1]，以及 [文字](https://example.com) 和 【注】，另有 array[i]。'
  for (const streaming of [false, true]) {
    const html = stripFade(renderChatMarkdown(text, { ...options, streaming }))
    assert.match(html, /\[1\]/)
    assert.match(html, /<a href="https:\/\/example\.com">文字<\/a>/)
    assert.match(html, /【注】/)
    assert.match(html, /array\[i\]/)
  }
})

test('renderChatMarkdown hides a streaming bracket tail and settles once complete (AC4)', () => {
  const partial = '有效期内[ref id="c'
  const hidden = stripFade(renderChatMarkdown(partial, { ...options, streaming: true }))
  assert.match(hidden, /有效期内/)
  assert.doesNotMatch(hidden, /ref id=/)

  const settled = renderChatMarkdown(partial + '2"]。', { ...options, streaming: false })
  assert.equal(settled.trimEnd(), '<p>有效期内。</p>')
})

test('renderChatMarkdown output is unchanged for content without markers (C4)', () => {
  const plain = [
    '# 标题',
    '',
    '正文 **粗体** 与 [链接](https://example.com)、![图](https://example.com/a.png)、[1]。',
    '',
    '- 项一',
    '- 项二',
    '',
    '```js',
    'a[0] < b[1]',
    '```',
  ].join('\n')
  const card = '引用 <kb doc="guide.pdf" chunk_id="chunk-1" /> 结束'
  const expectedCard = '<p>引用 <span class="citation citation-kb" data-kb-id="" data-chunk-id="chunk-1" data-doc="guide.pdf" role="button" tabindex="0"><span class="citation-icon citation-icon--book" aria-hidden="true"></span><span class="citation-text">guide.pdf</span><span class="citation-tip"><span class="tip-loading">…</span></span></span> 结束</p>'
  for (const streaming of [false, true]) {
    const html = stripFade(renderChatMarkdown(plain, { ...options, streaming }))
    assert.match(html, /<h1>标题<\/h1>/)
    assert.match(html, /<a href="https:\/\/example\.com">链接<\/a>/)
    assert.match(html, /\[1\]/)
    assert.match(html, /<li>项一<\/li>/)
    assert.match(html, /a\[0\] &lt; b\[1\]/)
    assert.equal(
      stripFade(renderChatMarkdown(card, { ...options, streaming })).trimEnd(),
      expectedCard,
    )
  }
})

test('session export and add-to-knowledge drop residual markers too', () => {
  const labels = { sessionId: 's', exportedAt: 'e', user: '用户', assistant: '助手', attachments: '附件', references: '引用' }
  const markdown = buildSessionMarkdown({
    sessionId: 's',
    title: 't',
    labels,
    messages: [{ role: 'assistant', content: '结论[ref id="c2"]。' }],
  })
  assert.doesNotMatch(markdown, /ref id=/)
  assert.match(markdown, /结论。/)

  const saved = stripCitationTagsForMarkdown('结论[ref id="c2"]。<kb doc="a.pdf" chunk_id="x" />')
  assert.equal(saved.text, '结论。')
  assert.deepEqual(saved.sources, ['a.pdf'])
})

function stripFade(html: string): string {
  return html.replace(/<span class="stream-fade-tail">([\s\S]*?)<\/span>/g, '$1')
}
