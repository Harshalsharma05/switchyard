// Live Ops load simulator (Step 4.4): browser-generated concurrent traffic
// against the gateway with a live readout. Bounded and clearly labelled as
// indicative, not a benchmark.
import { useState } from 'react'
import { useAuth } from '../hooks/useAuth.js'
import { MAX_CONCURRENCY, MAX_DURATION_S, useLoadSim } from '../hooks/useLoadSim.js'
import { EmptyState } from './states.jsx'
import { formatCount, formatMs } from '../utils/format.js'

function Stat({ label, value, tone }) {
  return (
    <div className={`sim-stat ${tone ? `sim-stat-${tone}` : ''}`}>
      <span className="sim-stat-label">{label}</span>
      <span className="sim-stat-value num">{value}</span>
    </div>
  )
}

export default function LoadSimulator() {
  const { me, getKey } = useAuth()
  const models = me?.allowed_models ?? []
  const { running, stats, start, stop } = useLoadSim(getKey)

  const [model, setModel] = useState(models[0] ?? '')
  const [concurrency, setConcurrency] = useState(5)
  const [duration, setDuration] = useState(15)

  const clamp = (n, max) => Math.max(1, Math.min(max, Math.floor(Number(n) || 1)))
  const c = stats.counts
  const started = running || stats.completed > 0

  if (models.length === 0) {
    return <EmptyState>This team has no allowed models, so there is nothing to send traffic to.</EmptyState>
  }

  return (
    <div className="sim">
      <div className="sim-controls">
        <label className="sim-field">
          <span>Model</span>
          <select value={model} onChange={(e) => setModel(e.target.value)} disabled={running}>
            {models.map((m) => <option key={m} value={m}>{m}</option>)}
          </select>
        </label>
        <label className="sim-field">
          <span>Concurrency</span>
          <input
            type="number" min="1" max={MAX_CONCURRENCY} className="num"
            value={concurrency} disabled={running}
            onChange={(e) => setConcurrency(e.target.value)}
            onBlur={() => setConcurrency((v) => clamp(v, MAX_CONCURRENCY))}
          />
        </label>
        <label className="sim-field">
          <span>Duration (s)</span>
          <input
            type="number" min="1" max={MAX_DURATION_S} className="num"
            value={duration} disabled={running}
            onChange={(e) => setDuration(e.target.value)}
            onBlur={() => setDuration((v) => clamp(v, MAX_DURATION_S))}
          />
        </label>
        {running ? (
          <button type="button" className="sim-stop" onClick={stop}>Stop</button>
        ) : (
          <button
            type="button" className="sim-start"
            disabled={!model}
            onClick={() => start(model, clamp(concurrency, MAX_CONCURRENCY), clamp(duration, MAX_DURATION_S))}
          >
            Start
          </button>
        )}
      </div>

      {started ? (
        <div className="sim-readout">
          <Stat label="Elapsed" value={`${(stats.elapsedMs / 1000).toFixed(1)}s`} />
          <Stat label="RPS" value={stats.rps.toFixed(1)} />
          <Stat label="Requests" value={formatCount(stats.completed)} />
          <Stat label="2xx" value={formatCount(c.ok)} tone="ok" />
          <Stat label="429" value={formatCount(c.rl)} tone="warn" />
          <Stat label="402" value={formatCount(c.budget)} tone="warn" />
          <Stat label="503" value={formatCount(c.unavailable)} tone="err" />
          <Stat label="other" value={formatCount(c.other)} tone={c.other > 0 ? 'err' : undefined} />
          <Stat label="p50" value={stats.p50 == null ? '—' : `${formatMs(stats.p50, 0)}ms`} />
          <Stat label="p95" value={stats.p95 == null ? '—' : `${formatMs(stats.p95, 0)}ms`} />
          <Stat label="p99" value={stats.p99 == null ? '—' : `${formatMs(stats.p99, 0)}ms`} />
        </div>
      ) : (
        <p className="sim-idle">Set concurrency and duration, then Start to send live traffic through the gateway.</p>
      )}

      <p className="sim-note">
        Browser-generated load — indicative only. Authoritative throughput and
        overhead numbers come from the committed k6 script (Phase 10), not from
        this tool. Capped at {MAX_CONCURRENCY} concurrent and {MAX_DURATION_S}s.
      </p>
    </div>
  )
}
