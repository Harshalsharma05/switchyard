// One lean request for the Live Ops load simulator (Step 4.4). Non-streaming
// and a tiny max_tokens — the point is exercising the gateway's request path
// under concurrency, not generating text. Returns just the status and the
// round-trip time; a transport failure is reported as status 0.
const ENDPOINT = '/v1/chat/completions'
const PROMPT = 'Reply with the single word: ok.'

export async function fireOne(key, model, signal) {
  const started = performance.now()
  try {
    const resp = await fetch(ENDPOINT, {
      method: 'POST',
      headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model,
        stream: false,
        max_tokens: 4,
        messages: [{ role: 'user', content: PROMPT }],
      }),
      signal,
    })
    // Drain the body so the connection can be reused by the next request.
    await resp.text()
    return { status: resp.status, ms: performance.now() - started }
  } catch (err) {
    if (err.name === 'AbortError') throw err
    return { status: 0, ms: performance.now() - started }
  }
}
