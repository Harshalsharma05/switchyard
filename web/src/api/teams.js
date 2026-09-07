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

// Mints a new key and invalidates the old one. The plaintext key is in the
// response body ONCE ({ api_key, key, warning }) and is never retrievable again.
export function rotateTeamKey(key, id, signal) {
  return request(`/admin/teams/${id}/key/rotate`, { key, method: 'POST', signal })
}

// Removes the team's key entirely; nothing authenticates as the team until it
// is rotated a new one.
export function revokeTeamKey(key, id, signal) {
  return request(`/admin/teams/${id}/key`, { key, method: 'DELETE', signal })
}
