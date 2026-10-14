/** Base URL of the internal docs site; configured via VITE_DOCS_BASE_URL at build time.
 * When unset, docs entries are hidden across the UI (docsUrl returns ''). */
export const DOCS_BASE_URL = resolveDocsBaseUrl(import.meta.env?.VITE_DOCS_BASE_URL)

/** Normalizes the configured base URL: trimmed, trailing slash enforced, '' when unset. */
export function resolveDocsBaseUrl(raw: unknown): string {
  if (typeof raw !== 'string') return ''
  const trimmed = raw.trim()
  if (!trimmed) return ''
  return trimmed.replace(/\/+$/, '') + '/'
}

/**
 * Doc pages linked from the UI, relative to VITE_DOCS_BASE_URL.
 */
export const DOC_PAGES = {
  home: '',
  tenantAuth: '03-features/01-tenant-auth',
  models: '03-features/06-models',
  modelsCompat: '03-features/06-models#协议兼容覆盖-compat-json',
  knowledgeGraph: '03-features/09-knowledge-graph',
  imIntegration: '03-features/12-im-integration',
  apiOverview: '04-api/01-api-overview',
  troubleshootingMigrations: '01-getting-started/05-troubleshooting#database-migrations',
  sandboxDeployment: '06-development/04-sandbox-deployment',
} as const

export type DocPage = keyof typeof DOC_PAGES

/** Returns the docs URL for a page, or '' when VITE_DOCS_BASE_URL is not configured. */
export function docsUrl(page: DocPage): string {
  if (!DOCS_BASE_URL) return ''
  return DOCS_BASE_URL + DOC_PAGES[page]
}
