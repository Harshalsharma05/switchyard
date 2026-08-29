// Live provider health and per-model circuit breaker state, read in-process by
// the gateway rather than from Prometheus.
import { request } from './client.js'

export function fetchProviderHealth(key, signal) {
  return request('/admin/providers/health', { key, signal })
}
