// The newest request-log rows, updating in place. Each row is keyed by request
// id, so a new row is a newly mounted element and gets its flash from a CSS
// mount animation — no bookkeeping needed to work out which rows are new.
// Insertion pauses while the pointer is over the table so a row cannot scroll
// away mid-read.
import { useState } from 'react'
import { StatusCode } from './primitives.jsx'
import { EmptyState } from './states.jsx'
import { formatClock, formatMs, formatUSD, middleTruncate } from '../utils/format.js'

export default function LiveFeed({ rows, showTeam }) {
  // Non-null while hovering: the snapshot taken when the pointer arrived.
  const [frozen, setFrozen] = useState(null)
  const shown = frozen ?? rows ?? []

  if (!shown.length) {
    return <EmptyState>No requests logged yet. Send one from the Playground and it will appear here.</EmptyState>
  }

  return (
    <div
      className="feed-wrap"
      onMouseEnter={() => setFrozen(rows ?? [])}
      onMouseLeave={() => setFrozen(null)}
    >
      <table className="table feed-table">
        <thead>
          <tr>
            <th>Time</th>
            <th>Request</th>
            {showTeam && <th>Team</th>}
            <th>Provider</th>
            <th>Model</th>
            <th>Status</th>
            <th className="ta-r">Latency</th>
            <th className="ta-r">Overhead</th>
            <th className="ta-r">Cost</th>
          </tr>
        </thead>
        <tbody>
          {shown.map((r) => (
            <tr key={r.id}>
              <td className="num">{formatClock(r.timestamp)}</td>
              <td className="num" title={r.id}>{middleTruncate(r.id)}</td>
              {showTeam && <td>{r.team_id}</td>}
              <td>{r.provider || '—'}</td>
              <td className="num" title={r.served_model}>{r.served_model || '—'}</td>
              <td>
                <StatusCode code={r.status_code} />
                {r.fallback && <span className="feed-flag">fallback</span>}
              </td>
              <td className="num ta-r">{formatMs(r.latency_ms, 0)}<span className="feed-unit">ms</span></td>
              <td className="num ta-r">{formatMs(r.overhead_ms)}<span className="feed-unit">ms</span></td>
              <td className="num ta-r">{formatUSD(r.cost_usd)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {frozen && <p className="feed-paused">Paused while hovering</p>}
    </div>
  )
}
