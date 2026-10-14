import { strict as assert } from 'node:assert'
import { test } from 'node:test'
import { DOC_PAGES, docsUrl, resolveDocsBaseUrl } from './docsUrl'

test('resolveDocsBaseUrl normalizes the configured value', () => {
  assert.equal(resolveDocsBaseUrl(undefined), '')
  assert.equal(resolveDocsBaseUrl(''), '')
  assert.equal(resolveDocsBaseUrl('   '), '')
  assert.equal(resolveDocsBaseUrl('https://docs.internal.example.com'), 'https://docs.internal.example.com/')
  assert.equal(resolveDocsBaseUrl('https://docs.internal.example.com/'), 'https://docs.internal.example.com/')
  assert.equal(resolveDocsBaseUrl(' https://docs.internal.example.com// '), 'https://docs.internal.example.com/')
  assert.equal(resolveDocsBaseUrl(42), '')
})

test('every doc page is a relative path without a leading slash', () => {
  for (const [key, target] of Object.entries(DOC_PAGES)) {
    assert.equal(target.startsWith('/'), false, `${key}: ${target}`)
  }
})

test('docsUrl returns an absolute URL when a base is configured, empty otherwise', () => {
  // VITE_DOCS_BASE_URL is unset in the test environment, so entries must be hidden.
  assert.equal(docsUrl('models'), '')
  const base = resolveDocsBaseUrl('https://docs.internal.example.com')
  assert.equal(base + DOC_PAGES.models, 'https://docs.internal.example.com/03-features/06-models')
})
