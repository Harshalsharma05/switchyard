// Circuit breaker state per provider+model. A breaker only exists once a
// request has been routed through it, so an empty panel is a real state and
// says so rather than showing nothing.
import { StatusPill } from './primitives.jsx'
import { EmptyState } from './states.jsx'

export default function BreakerPanel({ providers }) {
  const rows = (providers ?? []).flatMap((p) =>
    (p.breakers ?? []).map((b) => ({ key: `${p.provider}/${b.model}`, provider: p.provider, ...b })),
  )

  if (!rows.length) {
    return <EmptyState>No breakers built yet — one appears per provider and model once traffic has flowed through it.</EmptyState>
  }

  return (
    <ul className="plist">
      {rows.map((r) => (
        <li key={r.key} className="plist-row">
          <span className="plist-name num" title={r.key}>{r.provider}</span>
          <span className="plist-sub num" title={r.model}>{r.model}</span>
          <StatusPill status={r.state} />
        </li>
      ))}
    </ul>
  )
}
