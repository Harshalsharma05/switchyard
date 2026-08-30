// Step 2.5's live-update mechanism: polling, not SSE. The gateway's summary
// endpoint already caches for a few seconds, so N open tabs cost one Prometheus
// query per cache window regardless of N — which is what makes polling adequate
// here and an SSE connection per tab unnecessary.
//
// Two behaviours the plan requires and this hook owns: updates pause while the
// tab is hidden, and repeated failures back off exponentially instead of
// hammering a gateway that is down.
import { useCallback, useEffect, useRef, useState } from 'react'
import { clearConnection, reportConnection } from './connectionStore.js'

const MAX_BACKOFF_MS = 30_000

let nextId = 0

// `fetcher` receives an AbortSignal and returns a promise. It must be stable
// (wrap it in useCallback) — a change to it restarts the poll, which is exactly
// what should happen when something like the time range changes.
// `ignoreError` marks an error as expected and terminal: the poll stops, the
// error is surfaced for the caller to read, and the connection indicator is
// left alone — used for a 404 that means "this endpoint isn't here", which
// retrying every interval would only misreport as a bad connection.
export function usePolling(fetcher, { interval = 5000, enabled = true, ignoreError } = {}) {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(true)
  const [id] = useState(() => `poll-${nextId++}`)

  const timerRef = useRef(null)
  const abortRef = useRef(null)
  const failuresRef = useRef(0)

  // Bumped to force an immediate re-poll on demand (the retry controls).
  const [nonce, setNonce] = useState(0)
  const refresh = useCallback(() => setNonce((n) => n + 1), [])

  useEffect(() => {
    if (!enabled) {
      clearConnection(id)
      return
    }
    let cancelled = false

    const clearTimer = () => {
      if (timerRef.current) {
        clearTimeout(timerRef.current)
        timerRef.current = null
      }
    }

    const schedule = (delay) => {
      clearTimer()
      timerRef.current = setTimeout(run, delay)
    }

    async function run() {
      // Hidden tabs do not poll at all — check again when visibility returns.
      if (document.hidden) {
        schedule(interval)
        return
      }

      abortRef.current?.abort()
      const ac = new AbortController()
      abortRef.current = ac

      try {
        const result = await fetcher(ac.signal)
        if (cancelled || ac.signal.aborted) return
        failuresRef.current = 0
        setData(result)
        setError(null)
        setLoading(false)
        reportConnection(id, 'live')
        schedule(interval)
      } catch (err) {
        if (cancelled || ac.signal.aborted || err.name === 'AbortError') return
        if (ignoreError?.(err)) {
          setError(err)
          setLoading(false)
          clearConnection(id)
          return // no reschedule — the poll stops here
        }
        failuresRef.current += 1
        setError(err)
        setLoading(false)
        // Last good data is deliberately kept on screen — a stale number under a
        // visible "reconnecting" indicator beats a screen that empties itself
        // the moment one poll fails.
        reportConnection(id, failuresRef.current >= 3 ? 'disconnected' : 'reconnecting')
        schedule(Math.min(interval * 2 ** failuresRef.current, MAX_BACKOFF_MS))
      }
    }

    // Poll immediately when the tab becomes visible again rather than waiting
    // out whatever delay was pending.
    const onVisibility = () => {
      if (!document.hidden) schedule(0)
    }
    document.addEventListener('visibilitychange', onVisibility)

    run()

    return () => {
      cancelled = true
      clearTimer()
      abortRef.current?.abort()
      document.removeEventListener('visibilitychange', onVisibility)
      clearConnection(id)
    }
  }, [fetcher, enabled, interval, nonce, id, ignoreError])

  return { data, error, loading, refresh }
}
