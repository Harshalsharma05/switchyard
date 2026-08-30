// Step 4.4's browser load simulator. Spawns `concurrency` workers, each firing
// requests back-to-back at the gateway until the duration elapses, and rolls up
// a live readout. A demo instrument, not a benchmark — the authoritative
// numbers come from the committed k6 script (Phase 10).
import { useCallback, useEffect, useRef, useState } from 'react'
import { fireOne } from '../api/loadsim.js'

// Hard ceilings so the tool can never be pointed at a real provider quota hard
// enough to matter.
export const MAX_CONCURRENCY = 20
export const MAX_DURATION_S = 60

const emptyCounts = () => ({ ok: 0, rl: 0, budget: 0, unavailable: 0, other: 0 })

const emptyStats = () => ({
  elapsedMs: 0,
  completed: 0,
  rps: 0,
  counts: emptyCounts(),
  p50: null,
  p95: null,
  p99: null,
})

function percentile(sorted, p) {
  if (sorted.length === 0) return null
  const i = Math.min(sorted.length - 1, Math.floor((p / 100) * sorted.length))
  return sorted[i]
}

function bucket(status) {
  if (status >= 200 && status < 300) return 'ok'
  if (status === 429) return 'rl'
  if (status === 402) return 'budget'
  if (status === 503) return 'unavailable'
  return 'other'
}

export function useLoadSim(getKey) {
  const [running, setRunning] = useState(false)
  const [stats, setStats] = useState(emptyStats)
  const acRef = useRef(null)
  const accRef = useRef(null) // { samples: number[], counts, startedAt } — mutated per request, not state

  const snapshot = useCallback(() => {
    const acc = accRef.current
    if (!acc) return
    const elapsedMs = performance.now() - acc.startedAt
    const sorted = [...acc.samples].sort((a, b) => a - b)
    setStats({
      elapsedMs,
      completed: acc.samples.length,
      rps: elapsedMs > 0 ? acc.samples.length / (elapsedMs / 1000) : 0,
      counts: { ...acc.counts },
      p50: percentile(sorted, 50),
      p95: percentile(sorted, 95),
      p99: percentile(sorted, 99),
    })
  }, [])

  const stop = useCallback(() => acRef.current?.abort(), [])

  const start = useCallback(
    (model, concurrency, durationS) => {
      const c = Math.max(1, Math.min(MAX_CONCURRENCY, Math.floor(concurrency)))
      const d = Math.max(1, Math.min(MAX_DURATION_S, Math.floor(durationS)))

      const ac = new AbortController()
      acRef.current = ac
      accRef.current = { samples: [], counts: emptyCounts(), startedAt: performance.now() }
      setStats(emptyStats())
      setRunning(true)

      const deadline = performance.now() + d * 1000
      const ticker = setInterval(snapshot, 250)

      const worker = async () => {
        while (performance.now() < deadline && !ac.signal.aborted) {
          try {
            const { status, ms } = await fireOne(getKey(), model, ac.signal)
            accRef.current.samples.push(ms)
            accRef.current.counts[bucket(status)] += 1
          } catch {
            return // AbortError — the run was stopped
          }
        }
      }

      Promise.all(Array.from({ length: c }, worker)).finally(() => {
        clearInterval(ticker)
        snapshot()
        setRunning(false)
        acRef.current = null
      })
    },
    [getKey, snapshot],
  )

  // Leaving the screen mid-run stops the traffic.
  useEffect(() => () => acRef.current?.abort(), [])

  return { running, stats, start, stop }
}
