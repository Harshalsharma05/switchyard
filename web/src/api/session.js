// Identity calls. The access token is httpOnly, so the console cannot decode
// it -- session state comes from GET /auth/me, never from reading a cookie.
import { request } from './client.js'

export function fetchMe(signal) {
  return request('/auth/me', { signal })
}

export function logout() {
  return request('/auth/logout', { method: 'POST' })
}

// Where the browser goes to start a Google sign-in. A full navigation, not a
// fetch: the flow leaves this origin and comes back.
export const GOOGLE_SIGNIN_PATH = '/auth/google'
