// Project list and management, admin-only. Every mutation is audit-logged
// server-side before it is applied. The API and schema still call these
// "teams" — Step 3.1 renames the UI label only, so every call here still
// hits /admin/teams.
import { request } from './client.js'

export function fetchProjects(signal) {
  return request('/admin/teams', { signal })
}

// body: { name, organization_id?, priority, rpm, tpm, monthly_budget_usd,
// allowed_providers, allowed_models, is_admin }. The ID is derived from the name
// server-side. The response carries the plaintext key ONCE, in the same shape as
// a rotation ({ team, api_key, key, warning }).
export function createProject(body, signal) {
  return request('/admin/teams', { method: 'POST', body, signal })
}

// Soft delete: the project's key stops working immediately and its ID is never
// reused. 204 on success.
export function deleteProject(id, signal) {
  return request(`/admin/teams/${id}`, { method: 'DELETE', signal })
}

// patch carries only the fields being changed: { rpm?, tpm?, monthly_budget_usd? }.
export function patchProject(id, patch, signal) {
  return request(`/admin/teams/${id}`, { method: 'PATCH', body: patch, signal })
}

export function resetProjectBudget(id, signal) {
  return request(`/admin/teams/${id}/reset-budget`, { method: 'POST', signal })
}

// Mints a new key and invalidates the old one. The plaintext key is in the
// response body ONCE ({ api_key, key, warning }) and is never retrievable again.
export function rotateProjectKey(id, signal) {
  return request(`/admin/teams/${id}/key/rotate`, { method: 'POST', signal })
}

// Removes the project's key entirely; nothing authenticates as the project until
// it is rotated a new one.
export function revokeProjectKey(id, signal) {
  return request(`/admin/teams/${id}/key`, { method: 'DELETE', signal })
}
