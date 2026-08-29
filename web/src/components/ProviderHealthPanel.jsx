// Per-provider status, latency, and error rate. Live, from the gateway's own
// health monitor rather than from Prometheus.
import { StatusPill } from './primitives.jsx'
import { EmptyState } from './states.jsx'
import { formatMs, formatPercent } from '../utils/format.js'

export default function ProviderHealthPanel({ providers }) {
  if (!providers?.length) {
    return <EmptyState>No providers are configured.</EmptyState>
  }
  return (
    <ul className="plist">
      {providers.map((p) => (
        <li key={p.provider} className="plist-row">
          <span className="plist-name">{p.provider}</span>
          <StatusPill status={p.status} />
          <span className="plist-metrics num">
            {formatMs(p.p99_latency_ms, 0)}<span className="plist-unit">ms</span>
            {' · '}
            {formatPercent(p.error_rate, 1)}<span className="plist-unit">err</span>
          </span>
        </li>
      ))}
    </ul>
  )
}
