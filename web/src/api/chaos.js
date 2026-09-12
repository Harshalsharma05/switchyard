// Step 7.5's fault-injection harness. Admin-only, and the gateway answers 404
// on every route unless SWITCHYARD_ENV=dev and the chaos flag are both set.
import { request } from './client.js'

export function fetchChaos(signal) {
  return request('/admin/chaos', { signal })
}

// The whole rule set is replaced on every call — there is no add/remove.
export function setChaosRules(rules) {
  return request('/admin/chaos', { method: 'POST', body: { rules } })
}

export function clearChaos() {
  return request('/admin/chaos', { method: 'DELETE' })
}
