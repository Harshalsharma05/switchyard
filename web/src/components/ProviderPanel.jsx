// Live Ops provider panel: one card per provider with status, latency, error
// rate, last check, and recent status transitions with their reasons.
import { StatusPill } from './primitives.jsx'
import { EmptyState } from './states.jsx'
import ProviderChaosControl from './ProviderChaosControl.jsx'
import { formatMs, formatPercent, formatRelative } from '../utils/format.js'

// Most recent transitions first; the endpoint returns them oldest-first.
function recentTransitions(history) {
  return [...(history ?? [])].reverse().slice(0, 5)
}

export default function ProviderPanel({ providers }) {
  if (!providers?.length) {
    return <EmptyState>No providers are configured.</EmptyState>
  }
  return (
    <div className="provider-grid">
      {providers.map((p) => (
        <article key={p.provider} className="provider-card">
          <header className="provider-card-head">
            <span className="provider-card-name">{p.provider}</span>
            <StatusPill status={p.status} />
          </header>

          <dl className="provider-card-metrics">
            <div>
              <dt>p99 latency</dt>
              <dd className="num">{formatMs(p.p99_latency_ms, 0)}<span className="unit">ms</span></dd>
            </div>
            <div>
              <dt>error rate</dt>
              <dd className="num">{formatPercent(p.error_rate, 1)}</dd>
            </div>
            <div>
              <dt>last check</dt>
              <dd>{formatRelative(p.last_check_at)}</dd>
            </div>
          </dl>

          <div className="provider-card-history">
            <span className="provider-card-history-label">Recent transitions</span>
            {recentTransitions(p.history).length === 0 ? (
              <p className="provider-card-history-empty">No status changes recorded.</p>
            ) : (
              <ul>
                {recentTransitions(p.history).map((t, i) => (
                  <li key={`${t.at}-${i}`}>
                    <span className="provider-card-history-when num">{formatRelative(t.at)}</span>
                    <span className="provider-card-history-move num">{t.from} → {t.to}</span>
                    <span className="provider-card-history-reason">{t.reason}</span>
                  </li>
                ))}
              </ul>
            )}
          </div>

          <ProviderChaosControl provider={p.provider} />
        </article>
      ))}
    </div>
  )
}
