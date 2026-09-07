// System info and the config-reload control (Step 6.4, Settings §4). Both
// admin-only — the gateway 403s a non-admin key.
import { request } from './client.js'

export function fetchSystem(key, signal) {
  return request('/admin/system', { key, signal })
}

// POST /admin/reload takes no body. On success it returns
// { status: 'reloaded', providers, teams }.
export function reloadConfig(key, signal) {
  return request('/admin/reload', { key, method: 'POST', signal })
}
