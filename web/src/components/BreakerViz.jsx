// Live Ops circuit-breaker visualisation: one three-state machine per
// provider+model, current state lit and the others dimmed, plus the per-provider
// manual reset from Part 1 Step 7.4.
import { useState } from 'react'
import { EmptyState } from './states.jsx'
import { resetProviderBreaker } from '../api/health.js'
import { formatMs } from '../utils/format.js'

const STATES = [
  { key: 'closed', label: 'Closed', tone: 'healthy' },
  { key: 'open', label: 'Open', tone: 'error' },
  { key: 'half_open', label: 'Half-open', tone: 'warn' },
]

// The line under the machine changes with the state — failures while closed,
// cooldown while open, probe status while half-open.
function BreakerDetail({ b }) {
  if (b.state === 'open') {
    return (
      <span>
        cooldown <span className="num">{formatMs(b.cooldown_remaining_ms, 0)}</span> ms left
        {' · '}
        <span className="num">{b.failures}</span>/<span className="num">{b.failure_threshold}</span> failures
      </span>
    )
  }
  if (b.state === 'half_open') {
    return (
      <span>
        {b.probe_in_flight ? 'probe in flight' : 'probe slot free'}
        {' · '}
        <span className="num">{b.success_streak}</span>/<span className="num">{b.success_threshold}</span> to close
      </span>
    )
  }
  return (
    <span>
      <span className="num">{b.failures}</span>/<span className="num">{b.failure_threshold}</span> failures in window
      {b.reopens > 0 && <> · <span className="num">{b.reopens}</span> reopens</>}
    </span>
  )
}

// ResetControl is inline (no modal) and never optimistic — it shows a pending
// state and only clears once the server confirms, per DESIGN.md's control-plane
// rule.
function ResetControl({ provider, getKey, onDone }) {
  const [phase, setPhase] = useState('idle') // idle | confirming | pending | error

  async function run() {
    setPhase('pending')
    try {
      await resetProviderBreaker(getKey(), provider)
      setPhase('idle')
      onDone()
    } catch {
      setPhase('error')
    }
  }

  if (phase === 'pending') return <span className="breaker-reset-note">Resetting…</span>
  if (phase === 'confirming') {
    return (
      <span className="breaker-reset-confirm">
        Reset all {provider} breakers?
        <button type="button" className="breaker-reset-yes" onClick={run}>Confirm</button>
        <button type="button" className="breaker-reset-no" onClick={() => setPhase('idle')}>Cancel</button>
      </span>
    )
  }
  return (
    <span className="breaker-reset-note">
      {phase === 'error' && <span className="breaker-reset-failed">Reset failed. </span>}
      <button type="button" className="breaker-reset" onClick={() => setPhase('confirming')}>
        Reset breakers
      </button>
    </span>
  )
}

export default function BreakerViz({ providers, getKey, onReset }) {
  const groups = (providers ?? [])
    .map((p) => ({ provider: p.provider, breakers: p.breakers ?? [] }))
    .filter((g) => g.breakers.length > 0)

  if (!groups.length) {
    return (
      <EmptyState>
        No breakers built yet — one appears per provider and model once traffic
        has flowed through it.
      </EmptyState>
    )
  }

  return (
    <div className="breaker-groups">
      {groups.map((g) => (
        <section key={g.provider} className="breaker-group">
          <header className="breaker-group-head">
            <span className="breaker-group-name">{g.provider}</span>
            <ResetControl provider={g.provider} getKey={getKey} onDone={onReset} />
          </header>

          {g.breakers.map((b) => (
            <div key={b.model} className="breaker-row">
              <span className="breaker-model num" title={b.model}>{b.model}</span>
              <div className="breaker-machine" role="img" aria-label={`breaker ${b.state}`}>
                {STATES.map((s) => (
                  <span
                    key={s.key}
                    className={`breaker-state ${b.state === s.key ? `breaker-state-on breaker-state-${s.tone}` : ''}`}
                  >
                    {s.label}
                  </span>
                ))}
              </div>
              <span className="breaker-detail">
                <BreakerDetail b={b} />
              </span>
            </div>
          ))}
        </section>
      ))}
    </div>
  )
}
