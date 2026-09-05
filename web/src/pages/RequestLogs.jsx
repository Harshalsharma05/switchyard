// Request Logs (Phase 5): a filterable, cursor-paginated table over
// /admin/requests. Filters live in the URL so a view is shareable and survives
// a refresh; all filtering is server-side. A row click opens the detail drawer.
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { fetchRequests } from '../api/requests.js'
import { Card, StatusCode } from '../components/primitives.jsx'
import LogFilters from '../components/LogFilters.jsx'
import RequestDrawer from '../components/RequestDrawer.jsx'
import { EmptyState, ErrorState, Loading } from '../components/states.jsx'
import { useAuth } from '../hooks/useAuth.js'
import { formatCostShort, formatDateTime, formatMs, middleTruncate } from '../utils/format.js'
import './RequestLogs.css'

// URL params that narrow the result set. `range` is handled separately — it is
// always present and maps to a `since` timestamp rather than a raw parameter.
const NARROWING = ['team', 'status', 'provider', 'model', 'cache', 'fallback']
const RANGE_MS = { '1h': 3_600e3, '24h': 86_400e3, '7d': 604_800e3 }

function ModelCell({ requested, served }) {
  if (requested && served && requested !== served) {
    return (
      <span className="num" title={`${requested} → ${served}`}>
        {requested}<span className="logs-arrow"> → </span>{served}
      </span>
    )
  }
  const m = served || requested
  return <span className="num" title={m}>{m || '—'}</span>
}

export default function RequestLogs() {
  const { getKey, isAdmin } = useAuth()
  const [searchParams, setSearchParams] = useSearchParams()

  const filters = useMemo(() => {
    const f = {}
    for (const k of NARROWING) {
      if (k === 'team' && !isAdmin) continue // a non-admin cannot filter by team
      const v = searchParams.get(k)
      if (v) f[k] = v
    }
    return f
  }, [searchParams, isAdmin])
  const range = searchParams.get('range') || '24h'

  // One cursor per page visited; '' is the first page. Keyset pagination, never
  // an offset. Any filter change resets the stack to the first page.
  const [cursors, setCursors] = useState([''])
  const cursor = cursors[cursors.length - 1]
  const [nonce, setNonce] = useState(0)
  const [state, setState] = useState({ loading: true, error: null, data: null })
  const [selected, setSelected] = useState(null)
  const closeDrawer = useCallback(() => setSelected(null), [])

  useEffect(() => {
    const ac = new AbortController()
    // `since` is a pinned lower bound derived from the range; computed here in
    // the effect because Date.now() is impure and must not run during render.
    const since = new Date(Date.now() - (RANGE_MS[range] ?? RANGE_MS['24h'])).toISOString()
    fetchRequests(getKey(), { cursor, filters: { since, ...filters }, signal: ac.signal })
      .then((data) => setState({ loading: false, error: null, data }))
      .catch((err) => {
        if (err.name === 'AbortError') return
        setState({ loading: false, error: err, data: null })
      })
    return () => ac.abort()
  }, [getKey, cursor, nonce, filters, range])

  const rows = state.data?.requests ?? []
  const hasNext = Boolean(state.data?.next_cursor)
  const hasPrev = cursors.length > 1
  const activeCount = Object.keys(filters).length

  // Loading is flagged from the handlers, not the effect — a synchronous
  // setState inside an effect body triggers a cascading render.
  const load = () => setState((s) => ({ ...s, loading: true }))

  const set = (key, value) => {
    load()
    setCursors([''])
    setSelected(null)
    setSearchParams((prev) => {
      const nextp = new URLSearchParams(prev)
      if (value && !(key === 'range' && value === '24h')) nextp.set(key, value)
      else nextp.delete(key)
      return nextp
    }, { replace: true })
  }
  const clearAll = () => {
    load()
    setCursors([''])
    setSelected(null)
    setSearchParams((prev) => {
      const nextp = new URLSearchParams(prev)
      NARROWING.forEach((k) => nextp.delete(k))
      return nextp
    }, { replace: true })
  }

  const next = () => { if (hasNext) { load(); setSelected(null); setCursors((c) => [...c, state.data.next_cursor]) } }
  const prev = () => { if (hasPrev) { load(); setSelected(null); setCursors((c) => c.slice(0, -1)) } }
  const retry = () => { load(); setNonce((n) => n + 1) }

  let body
  if (state.loading && !state.data) {
    body = <Loading rows={8} />
  } else if (state.error && !state.data) {
    body = state.error.type === 'request_log_disabled' ? (
      <EmptyState>The request log is not configured on this gateway.</EmptyState>
    ) : (
      <ErrorState message="Could not load the request log." onRetry={retry} />
    )
  } else if (!rows.length) {
    body = activeCount > 0 ? (
      <EmptyState action={<button type="button" className="logs-pager-btn" onClick={clearAll}>Clear filters</button>}>
        No requests match these filters in the selected time range.
      </EmptyState>
    ) : (
      <EmptyState>No requests logged yet. Send one from the Playground and it will appear here.</EmptyState>
    )
  } else {
    body = (
      <>
        <div className="logs-table-wrap">
          <table className="table logs-table">
            <thead>
              <tr>
                <th>Time</th>
                <th>Request</th>
                {isAdmin && <th>Team</th>}
                <th>Provider</th>
                <th>Model</th>
                <th>Routing</th>
                <th>Status</th>
                <th className="ta-r">Latency</th>
                <th className="ta-r">Overhead</th>
                <th className="ta-r">Tokens</th>
                <th className="ta-r">Cost</th>
                <th>Cache</th>
                <th className="ta-r">Quality</th>
                <th>Fallback</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr
                  key={r.id}
                  className={`logs-row ${selected?.id === r.id ? 'logs-row-active' : ''}`}
                  tabIndex={0}
                  aria-label={`Request ${r.id}`}
                  onClick={() => setSelected(r)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); setSelected(r) }
                  }}
                >
                  <td className="num">{formatDateTime(r.timestamp)}</td>
                  <td className="num" title={r.id}>{middleTruncate(r.id)}</td>
                  {isAdmin && <td>{r.team_id}</td>}
                  <td>{r.provider || '—'}</td>
                  <td><ModelCell requested={r.requested_model} served={r.served_model} /></td>
                  <td>
                    {r.routing_tier
                      ? <span className="logs-flag" title={r.routing_reason}>{r.routing_tier}</span>
                      : <span className="logs-muted">—</span>}
                  </td>
                  <td><StatusCode code={r.status_code} /></td>
                  <td className="num ta-r">{formatMs(r.latency_ms, 0)}<span className="logs-unit">ms</span></td>
                  <td className="num ta-r">{formatMs(r.overhead_ms)}<span className="logs-unit">ms</span></td>
                  <td className="num ta-r">{r.input_tokens} / {r.output_tokens}</td>
                  <td className="num ta-r">{formatCostShort(r.cost_micros)}</td>
                  <td>
                    {r.cache_hit == null
                      ? <span className="logs-muted">—</span>
                      : <span className={r.cache_hit ? 'logs-flag' : 'logs-muted'}>{r.cache_hit ? 'hit' : 'miss'}</span>}
                  </td>
                  <td className="num ta-r">
                    {r.quality_score == null
                      ? <span className="logs-muted">—</span>
                      : <span title={r.quality_sample_reason ? `sampled: ${r.quality_sample_reason.replace(/_/g, ' ')}` : undefined}>{r.quality_score.toFixed(1)}</span>}
                  </td>
                  <td>{r.fallback ? <span className="logs-flag">fallback</span> : <span className="logs-muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <div className="logs-pager">
          <span className="logs-pager-info num">
            Page {cursors.length} · {rows.length} row{rows.length === 1 ? '' : 's'}
          </span>
          <div className="logs-pager-btns">
            <button type="button" className="logs-pager-btn" onClick={prev} disabled={!hasPrev}>
              Previous
            </button>
            <button
              type="button"
              className="logs-pager-btn"
              onClick={next}
              disabled={!hasNext}
              title={hasNext ? undefined : 'You are on the last page'}
            >
              Next
            </button>
          </div>
        </div>
      </>
    )
  }

  return (
    <>
      <h1 className="page-title">Request logs</h1>
      <Card>
        <LogFilters
          getKey={getKey}
          isAdmin={isAdmin}
          filters={filters}
          range={range}
          set={set}
          clearAll={clearAll}
          activeCount={activeCount}
        />
        {body}
      </Card>
      {selected && <RequestDrawer row={selected} onClose={closeDrawer} />}
    </>
  )
}
