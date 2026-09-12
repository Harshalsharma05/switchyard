// Thin fetch wrapper. Every gateway call goes through here so cookie handling,
// CSRF, JSON, the error shape, and the access-token refresh are decided once.
// Paths are same-origin relative (/admin/…, /auth/…, /v1/…); Vite's dev proxy
// and nginx in production route them to the right port.

// ApiError carries the gateway's own error envelope ({error:{type,message}})
// so callers can branch on type (session_expired, org_scope_pending, …) rather
// than parsing a message string.
export class ApiError extends Error {
  constructor(status, type, message, detail) {
    super(message || `request failed with ${status}`)
    this.name = 'ApiError'
    this.status = status
    this.type = type
    this.detail = detail ?? null
  }
}

// onSessionLost is called when a refresh could not rescue a 401 — the session
// is genuinely gone. SessionProvider installs the handler; this module stays
// free of React so it can be imported anywhere.
let onSessionLost = null
export function setSessionLostHandler(fn) { onSessionLost = fn }

// The CSRF cookie is readable on purpose: a cross-origin script can send our
// cookies but cannot read them, so echoing this value in a header is proof the
// request came from our own page.
//
// Read at send time and never cached. It is reissued at sign-in, and a second
// tab must not hold a stale copy.
function csrfToken() {
  const match = document.cookie.match(/(?:^|;\s*)sy_csrf=([^;]*)/)
  return match ? decodeURIComponent(match[1]) : null
}

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS'])

async function send(path, { method = 'GET', body, signal } = {}) {
  const headers = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (!SAFE_METHODS.has(method)) {
    const token = csrfToken()
    if (token) headers['X-CSRF-Token'] = token
  }

  let resp
  try {
    resp = await fetch(path, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal,
      // Explicit rather than relying on the default: the session cookies are
      // the only credential this app has.
      credentials: 'same-origin',
    })
  } catch (err) {
    if (err.name === 'AbortError') throw err
    throw new ApiError(0, 'unreachable', 'could not reach the gateway')
  }

  const text = await resp.text()
  const data = text ? JSON.parse(text) : null
  return { resp, data }
}

function toError(resp, data) {
  const e = data && data.error
  return new ApiError(resp.status, e?.type ?? 'error', e?.message, e)
}

// refreshInFlight collapses concurrent refreshes into one.
//
// This is a correctness requirement, not an optimisation. The dashboard polls
// several endpoints at once, so at the 15-minute mark several 401s land
// together. If each fired its own refresh, the first would rotate the token and
// the rest would present one that had already been rotated -- which the gateway
// reads as a replayed token and answers by revoking EVERY session for the user.
// A naive implementation would therefore sign the user out of every browser,
// reliably, once per token lifetime.
let refreshInFlight = null

export function refreshSession() {
  refreshInFlight ??= send('/auth/refresh', { method: 'POST' })
    .then(({ resp, data }) => {
      if (!resp.ok) throw toError(resp, data)
      return true
    })
    .finally(() => { refreshInFlight = null })
  return refreshInFlight
}

// request performs one call, refreshing the access token and replaying once if
// the gateway says the session expired mid-flight. Callers never see that 401:
// the retry resolves with real data, so a chart keeps its points and a table
// does not blank.
export async function request(path, opts = {}) {
  const { _retried, ...rest } = opts
  const { resp, data } = await send(path, rest)

  if (resp.ok) return data
  if (resp.status !== 401 || _retried || path.startsWith('/auth/')) {
    throw toError(resp, data)
  }

  try {
    await refreshSession()
  } catch {
    // The refresh itself failed: the session is gone, not stale.
    onSessionLost?.()
    throw toError(resp, data)
  }

  // Exactly one retry. A second 401 is real.
  return request(path, { ...rest, _retried: true })
}
