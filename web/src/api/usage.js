// Usage & Cost data (Phase 6). Team spend comes from /admin/me (own team) or
// /admin/teams (admin, all teams); the cost trend and the Redis-vs-log
// reconciliation are their own endpoints.
import { request } from './client.js'

// range: 24h | 7d | 30d. by: provider | model | team (team is admin-only in
// practice — a non-admin's series only ever has its own team in it).
export function fetchCosts({ range = '24h', by = 'provider', team, signal } = {}) {
  const q = new URLSearchParams({ range, by })
  if (team) q.set('team', team)
  return request(`/admin/costs?${q}`, { signal })
}

// Admin-only: compares every team's live Redis budget counter against the sum
// of its logged request costs for the current month. team narrows to one
// project (Step 3.3).
export function fetchReconciliation({ team, signal } = {}) {
  const q = new URLSearchParams()
  if (team) q.set('team', team)
  const qs = q.toString()
  return request(`/admin/reconciliation${qs ? `?${qs}` : ''}`, { signal })
}

// "What did resilience cost you": the fallback cost delta over `range`, split
// into what fallbacks added and what they saved. Cache and routing savings are
// null until Phases 7 and 8. team narrows to one project (Step 3.3).
export function fetchAttribution({ range = '24h', team, signal } = {}) {
  const q = new URLSearchParams({ range })
  if (team) q.set('team', team)
  return request(`/admin/attribution?${q}`, { signal })
}
