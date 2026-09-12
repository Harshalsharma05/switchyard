// Audit log (Settings §3): every admin mutation, newest first, cursor-paginated.
// Backed by GET /admin/audit. Same keyset-cursor stack as Request Logs. The
// parent remounts this (via a key) after a mutation, so it always reopens on
// page 1 showing the action just taken.
import { useEffect, useState } from 'react'
import { fetchAudit } from '../api/audit.js'
import { EmptyState, ErrorState, Loading } from './states.jsx'
import { formatDateTime } from '../utils/format.js'

// before/after are field-level deltas ({} when the action has none). Render each
// changed field as "name: before → after" over the union of keys.
function Change({ before, after }) {
  const keys = [...new Set([...Object.keys(before ?? {}), ...Object.keys(after ?? {})])]
  if (keys.length === 0) return <span className="audit-muted">—</span>
  return (
    <span className="audit-change">
      {keys.map((k) => (
        <span key={k} className="audit-delta">
          <span className="audit-field">{k.replace(/_/g, ' ')}</span>
          <span className="num">{fmt(before?.[k])}</span>
          <span className="audit-arrow"> → </span>
          <span className="num">{fmt(after?.[k])}</span>
        </span>
      ))}
    </span>
  )
}

function fmt(v) {
  if (v === undefined || v === null) return '—'
  return String(v)
}

export default function AuditLog() {
  const [cursors, setCursors] = useState([''])
  const cursor = cursors[cursors.length - 1]
  const [nonce, setNonce] = useState(0)
  const [state, setState] = useState({ loading: true, error: null, data: null })

  useEffect(() => {
    const ac = new AbortController()
    fetchAudit({ cursor, signal: ac.signal })
      .then((data) => setState({ loading: false, error: null, data }))
      .catch((err) => {
        if (err.name === 'AbortError') return
        setState({ loading: false, error: err, data: null })
      })
    return () => ac.abort()
  }, [cursor, nonce])

  const rows = state.data?.entries ?? []
  const hasNext = Boolean(state.data?.next_cursor)
  const hasPrev = cursors.length > 1

  // Loading is flagged from the handlers, not the effect — a synchronous
  // setState in an effect body triggers a cascading render (same as RequestLogs).
  const load = () => setState((s) => ({ ...s, loading: true }))
  const next = () => { if (hasNext) { load(); setCursors((c) => [...c, state.data.next_cursor]) } }
  const prev = () => { if (hasPrev) { load(); setCursors((c) => c.slice(0, -1)) } }
  const retry = () => { load(); setCursors(['']); setNonce((n) => n + 1) }

  if (state.loading && !state.data) return <Loading rows={6} />
  if (state.error && !state.data) {
    return state.error.type === 'audit_log_disabled' ? (
      <EmptyState>The audit log is not configured on this gateway (no Postgres).</EmptyState>
    ) : (
      <ErrorState message="Could not load the audit log." onRetry={retry} />
    )
  }
  if (!rows.length) return <EmptyState>No admin actions recorded yet.</EmptyState>

  return (
    <>
      <div className="audit-wrap">
        <table className="table audit-table">
          <thead>
            <tr>
              <th>Time</th>
              <th>Actor</th>
              <th>Action</th>
              <th>Team</th>
              <th>Change</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((e) => (
              <tr key={e.id}>
                <td className="num">{formatDateTime(e.timestamp)}</td>
                <td className="num" title={e.actor_addr}>{e.actor}</td>
                <td><span className="audit-action">{e.action}</span></td>
                <td className="num">{e.target_team || '—'}</td>
                <td><Change before={e.before} after={e.after} /></td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="audit-pager">
        <span className="audit-pager-info num">Page {cursors.length} · {rows.length} row{rows.length === 1 ? '' : 's'}</span>
        <div className="audit-pager-btns">
          <button type="button" className="tt-btn" onClick={prev} disabled={!hasPrev}>Previous</button>
          <button type="button" className="tt-btn" onClick={next} disabled={!hasNext}>Next</button>
        </div>
      </div>
    </>
  )
}
