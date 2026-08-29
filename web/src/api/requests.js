// Request-log rows. Phase 5 adds filters and cursor pagination; Overview's live
// feed only ever needs the newest page.
import { request } from './client.js'

export function fetchRequests(key, { limit = 25, signal } = {}) {
  return request(`/admin/requests?limit=${limit}`, { key, signal })
}
