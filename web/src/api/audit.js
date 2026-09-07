// The operator audit log (Step 6.4, Settings §3). Admin-only, cursor-paginated
// newest-first. Returns { entries: [...], next_cursor? }.
import { request } from './client.js'

export function fetchAudit(key, { cursor = '', limit, signal } = {}) {
  const q = new URLSearchParams()
  if (cursor) q.set('cursor', cursor)
  if (limit) q.set('limit', String(limit))
  const qs = q.toString()
  return request(`/admin/audit${qs ? `?${qs}` : ''}`, { key, signal })
}
