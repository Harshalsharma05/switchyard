// Quality feedback (Phase 9.3). Admin-only: the two loops that keep the cache
// threshold and the routing classifier honest. Non-admin keys get 403.
import { request } from './client.js'

export function fetchQualityFeedback(key, { range = '7d', signal } = {}) {
  return request(`/admin/quality/feedback?range=${encodeURIComponent(range)}`, { key, signal })
}
