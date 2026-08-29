// Request-log rows. Phase 5 adds filters and cursor pagination; Overview's live
// feed only ever needs the newest page.
import { ApiError, request } from './client.js'

export function fetchRequests(key, { limit = 25, signal } = {}) {
  return request(`/admin/requests?limit=${limit}`, { key, signal })
}

export function fetchRequestById(key, id, signal) {
  return request(`/admin/requests/${id}`, { key, signal })
}

// The log row is written asynchronously after the response completes, so it is
// not queryable the instant a request returns. Poll a few times with growing
// gaps; resolve null if it never lands or the log is disabled (404 = not yet).
export async function awaitRequestRow(key, id, { signal } = {}) {
  for (const ms of [300, 500, 800, 1200, 2000]) {
    await new Promise((r) => setTimeout(r, ms))
    if (signal?.aborted) return null
    try {
      return await fetchRequestById(key, id, signal)
    } catch (err) {
      if (err.name === 'AbortError') return null
      if (!(err instanceof ApiError) || err.status !== 404) return null
    }
  }
  return null
}
