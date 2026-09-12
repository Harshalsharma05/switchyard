// Live provider health and per-model circuit breaker state, read in-process by
// the gateway rather than from Prometheus.
import { request } from './client.js'

export function fetchProviderHealth(signal) {
  return request('/admin/providers/health', { signal })
}

// Step 7.4's manual intervention: force every breaker for one provider closed.
export function resetProviderBreaker(provider) {
  return request(`/admin/providers/${encodeURIComponent(provider)}/breaker/reset`, { method: 'POST',
  })
}
