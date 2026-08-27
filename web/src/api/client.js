// Thin fetch wrapper. Every gateway call goes through here so the bearer key,
// JSON handling, and error shape are decided once. Paths are same-origin
// relative (/admin/…, /v1/…); Vite's dev proxy routes them to the right port.

// ApiError carries the gateway's own error envelope ({error:{type,message}})
// so callers can branch on type (invalid_api_key, admin_required, …) rather
// than parsing a message string.
export class ApiError extends Error {
  constructor(status, type, message) {
    super(message || `request failed with ${status}`)
    this.name = 'ApiError'
    this.status = status
    this.type = type
  }
}

// request performs one call. `key` is the team API key; when present it is
// sent as a bearer token, matching what the gateway expects on both surfaces.
export async function request(path, { key, method = 'GET', body, signal } = {}) {
  const headers = {}
  if (key) headers.Authorization = `Bearer ${key}`
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  let resp
  try {
    resp = await fetch(path, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal,
    })
  } catch (err) {
    if (err.name === 'AbortError') throw err
    // Network-level failure: the gateway is unreachable, not returning an error.
    throw new ApiError(0, 'unreachable', 'could not reach the gateway')
  }

  const text = await resp.text()
  const data = text ? JSON.parse(text) : null

  if (!resp.ok) {
    const e = data && data.error
    throw new ApiError(resp.status, e?.type ?? 'error', e?.message)
  }
  return data
}
