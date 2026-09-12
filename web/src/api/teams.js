// Team list and management, admin-only. Every mutation is audit-logged
// server-side before it is applied.
import { request } from './client.js'

export function fetchTeams(signal) {
  return request('/admin/teams', { signal })
}

// body: { name, organization_id?, priority, rpm, tpm, monthly_budget_usd,
// allowed_providers, allowed_models, is_admin }. The ID is derived from the name
// server-side. The response carries the plaintext key ONCE, in the same shape as
// a rotation ({ team, api_key, key, warning }).
export function createTeam(body, signal) {
  return request('/admin/teams', { method: 'POST', body, signal })
}

// Soft delete: the team's key stops working immediately and its ID is never
// reused. 204 on success.
export function deleteTeam(id, signal) {
  return request(`/admin/teams/${id}`, { method: 'DELETE', signal })
}

// patch carries only the fields being changed: { rpm?, tpm?, monthly_budget_usd? }.
export function patchTeam(id, patch, signal) {
  return request(`/admin/teams/${id}`, { method: 'PATCH', body: patch, signal })
}

export function resetTeamBudget(id, signal) {
  return request(`/admin/teams/${id}/reset-budget`, { method: 'POST', signal })
}

// Mints a new key and invalidates the old one. The plaintext key is in the
// response body ONCE ({ api_key, key, warning }) and is never retrievable again.
export function rotateTeamKey(id, signal) {
  return request(`/admin/teams/${id}/key/rotate`, { method: 'POST', signal })
}

// Removes the team's key entirely; nothing authenticates as the team until it
// is rotated a new one.
export function revokeTeamKey(id, signal) {
  return request(`/admin/teams/${id}/key`, { method: 'DELETE', signal })
}
