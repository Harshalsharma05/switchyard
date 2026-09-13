// Overview's (and Usage & Cost's roll-up) data. One call returns KPIs, chart
// series, and a provider-health snapshot; breaker state and transition
// history come from health.js.
import { request } from './client.js'

// team narrows to one project (Multi-user, Step 3.3's top-bar selector);
// omitted or falsy means every project in the caller's organisation.
export function fetchSummary(range, team, signal) {
  const q = new URLSearchParams({ range })
  if (team) q.set('team', team)
  return request(`/admin/summary?${q}`, { signal })
}
