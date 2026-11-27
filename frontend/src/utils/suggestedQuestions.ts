/**
 * 推荐问题请求失败时的排查信息。
 *
 * 推荐区在请求失败时保持隐藏（不弹提示），所以这里把能定位问题的错误码
 * 打到控制台：HTTP 状态码和后端的业务码。
 *
 * 常见取值：HTTP 404 表示智能体不存在，或者当前成员不能通过共享使用它
 * （共享已撤销、本空间已停用）；HTTP 403 多半是 API Key 的知识库范围不够。
 */
export function suggestedQuestionsErrorLabel(err: unknown): string {
  const e = (err ?? {}) as Record<string, any>
  const http = typeof e.status === 'number' ? e.status : e.$httpStatus
  const code = e?.error?.code
  const parts: string[] = []
  if (typeof http === 'number') parts.push(`HTTP ${http}`)
  if (code !== undefined && code !== null && code !== '') parts.push(`code ${String(code)}`)
  if (parts.length === 0) {
    parts.push(typeof e?.message === 'string' && e.message ? e.message : 'unknown error')
  }
  return parts.join(', ')
}
