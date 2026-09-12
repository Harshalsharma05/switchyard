// Overview's data. One call returns KPIs, chart series, and a provider-health
// snapshot; breaker state and transition history come from health.js.
import { request } from './client.js'

export function fetchSummary(range, signal) {
  return request(`/admin/summary?range=${encodeURIComponent(range)}`, { signal })
}
