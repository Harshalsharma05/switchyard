// Playground's call to POST /v1/chat/completions. Unlike api/client.js this
// reads the body as a stream so tokens render as they arrive (Step 3.1).
import { ApiError } from './client.js'

const ENDPOINT = '/v1/chat/completions'

function numberOr(s) {
  const n = Number(s)
  return s != null && s !== '' && !Number.isNaN(n) ? n : null
}

// The metadata the gateway reports in response headers. Latency, tokens, and
// cost are not here — those come from the request-log row, fetched by id.
function readMeta(resp) {
  const h = resp.headers
  return {
    requestId: h.get('X-Switchyard-Request-Id'),
    provider: h.get('X-Switchyard-Provider'),
    requestedModel: h.get('X-Switchyard-Requested-Model'),
    servedModel: h.get('X-Switchyard-Served-Model'),
    overheadMs: numberOr(h.get('X-Switchyard-Overhead-Ms')),
    fallback: h.get('X-Switchyard-Fallback') === 'true',
    // 'exact', 'semantic', or 'miss'. Absent when the gateway has no cache.
    cache: h.get('X-Switchyard-Cache'),
    // Embedding time, excluded from overheadMs above and reported separately.
    embedMs: numberOr(h.get('X-Switchyard-Embed-Ms')),
    // Step 8.3. Both null unless the caller asked to be routed, which is why
    // absence reads as "not routed" rather than as a failure to decide.
    routeTier: h.get('X-Switchyard-Route-Tier'),
    routeReason: h.get('X-Switchyard-Route-Reason'),
  }
}

async function toApiError(resp) {
  const text = await resp.text()
  let body = null
  try { body = text ? JSON.parse(text) : null } catch { /* not JSON */ }
  const e = (body && body.error) ?? {}
  const err = new ApiError(resp.status, e.type ?? 'error', e.message, e)

  // Rate-limit responses carry their backoff and bucket state in headers, not
  // the body; keep them on the error so Playground can render them (Step 3.3).
  const retryAfter = numberOr(resp.headers.get('Retry-After'))
  if (retryAfter != null) err.retryAfter = retryAfter
  if (resp.headers.get('X-RateLimit-Remaining') != null) {
    err.rateLimit = {
      limit: numberOr(resp.headers.get('X-RateLimit-Limit')),
      remaining: numberOr(resp.headers.get('X-RateLimit-Remaining')),
      reset: numberOr(resp.headers.get('X-RateLimit-Reset')),
    }
  }
  return err
}

// Reads the gateway's SSE stream: `data: {json}` blocks separated by a blank
// line, ending with `data: [DONE]`. A mid-stream `{"error":{…}}` event closes
// the stream without [DONE].
async function consumeStream(body, onDelta) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  let full = ''

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })

    let sep
    while ((sep = buffer.indexOf('\n\n')) !== -1) {
      const frame = buffer.slice(0, sep).trim()
      buffer = buffer.slice(sep + 2)
      if (!frame.startsWith('data:')) continue

      const payload = frame.slice(5).trim()
      if (payload === '[DONE]') return full

      const evt = JSON.parse(payload)
      if (evt.error) {
        // Mid-stream failure: headers and some tokens already went out, so there
        // is no HTTP status to carry it — status 0 marks that.
        throw new ApiError(0, evt.error.type ?? 'stream_error', evt.error.message, evt.error)
      }
      const delta = evt.choices?.[0]?.delta?.content ?? ''
      if (delta) { full += delta; onDelta(delta) }
    }
  }
  return full
}

// sendChat resolves with the full response text. onMeta fires once with the
// header metadata as soon as the response starts; onDelta fires per chunk while
// streaming, and once with the whole message when streaming is off.
export async function sendChat({ key, prompt, model, stream, signal, onDelta, onMeta }) {
  let resp
  try {
    resp = await fetch(ENDPOINT, {
      method: 'POST',
      headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ model, stream, messages: [{ role: 'user', content: prompt }] }),
      signal,
    })
  } catch (err) {
    if (err.name === 'AbortError') throw err
    throw new ApiError(0, 'unreachable', 'could not reach the gateway')
  }

  if (!resp.ok) throw await toApiError(resp)
  onMeta?.(readMeta(resp))

  if (!stream) {
    const data = await resp.json()
    const content = data?.choices?.[0]?.message?.content ?? ''
    onDelta(content)
    return content
  }
  return consumeStream(resp.body, onDelta)
}
