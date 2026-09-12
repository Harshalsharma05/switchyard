// Request-log rows. Overview's live feed only ever needs the newest page;
// Request Logs pages back through history with the server's opaque cursor.
import { ApiError, request } from './client.js'

// `filters` maps straight onto the query API's parameters (team, status,
// provider, model, cache, fallback, since); empty values are dropped so the
// gateway sees only the filters that are actually set.
export function fetchRequests({ limit, cursor, filters = {}, signal } = {}) {
  const q = new URLSearchParams()
  if (limit) q.set('limit', String(limit))
  if (cursor) q.set('cursor', cursor)
  for (const [k, v] of Object.entries(filters)) {
    if (v) q.set(k, String(v))
  }
  const qs = q.toString()
  return request(`/admin/requests${qs ? `?${qs}` : ''}`, { signal })
}

export function fetchRequestById(id, signal) {
  return request(`/admin/requests/${id}`, { signal })
}

// The log row is written asynchronously after the response completes, so it is
// not queryable the instant a request returns. Poll a few times with growing
// gaps; resolve null if it never lands or the log is disabled (404 = not yet).
export async function awaitRequestRow(id, { signal } = {}) {
  for (const ms of [300, 500, 800, 1200, 2000]) {
    await new Promise((r) => setTimeout(r, ms))
    if (signal?.aborted) return null
    try {
      return await fetchRequestById(id, signal)
    } catch (err) {
      if (err.name === 'AbortError') return null
      if (!(err instanceof ApiError) || err.status !== 404) return null
    }
  }
  return null
}
