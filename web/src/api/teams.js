// Team list, admin-only. Phase 5 uses it for the Request Logs team filter;
// Phase 6's team management builds on the same resource.
import { request } from './client.js'

export function fetchTeams(key, signal) {
  return request('/admin/teams', { key, signal })
}
