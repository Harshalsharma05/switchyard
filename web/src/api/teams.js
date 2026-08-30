// Team list and management, admin-only. Every mutation goes through Part 1's
// existing audit-logged endpoints — no new paths.
import { request } from './client.js'

export function fetchTeams(key, signal) {
  return request('/admin/teams', { key, signal })
}

// patch carries only the fields being changed: { rpm?, tpm?, monthly_budget_usd? }.
export function patchTeam(key, id, patch, signal) {
  return request(`/admin/teams/${id}`, { key, method: 'PATCH', body: patch, signal })
}

export function resetTeamBudget(key, id, signal) {
  return request(`/admin/teams/${id}/reset-budget`, { key, method: 'POST', signal })
}
