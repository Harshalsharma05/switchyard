// Step 7.5's fault-injection harness. Admin-only, and the gateway answers 404
// on every route unless SWITCHYARD_ENV=dev and the chaos flag are both set.
import { request } from './client.js'

export function fetchChaos(key, signal) {
  return request('/admin/chaos', { key, signal })
}

// The whole rule set is replaced on every call — there is no add/remove.
export function setChaosRules(key, rules) {
  return request('/admin/chaos', { key, method: 'POST', body: { rules } })
}

export function clearChaos(key) {
  return request('/admin/chaos', { key, method: 'DELETE' })
}
