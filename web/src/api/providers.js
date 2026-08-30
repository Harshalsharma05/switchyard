// Configured providers and their model catalogue. Read-only operator surface,
// no key required; the Request Logs filter bar reads it to build its provider
// and model selects.
import { request } from './client.js'

export function fetchProviders(key, signal) {
  return request('/admin/providers', { key, signal })
}
