// The Request Logs filter bar (Step 5.2). Every filter is server-side; this
// component only chooses values and hands them up. Provider/model options come
// from the provider catalogue, team options from /admin/teams (admin only).
import { useEffect, useState } from 'react'
import { fetchProviders } from '../api/providers.js'
import { fetchTeams } from '../api/teams.js'

const STATUSES = [
  ['2xx', '2xx success'],
  ['4xx', '4xx client error'],
  ['5xx', '5xx server error'],
  ['429', '429 rate limited'],
  ['402', '402 budget'],
  ['503', '503 unavailable'],
]

const RANGES = [
  ['1h', 'Last hour'],
  ['24h', 'Last 24 hours'],
  ['7d', 'Last 7 days'],
]

function Field({ label, value, onChange, children }) {
  return (
    <label className="logf-field">
      <span className="logf-label">{label}</span>
      <select className="logf-select" value={value} onChange={(e) => onChange(e.target.value)}>
        {children}
      </select>
    </label>
  )
}

export default function LogFilters({ getKey, isAdmin, filters, range, set, clearAll, activeCount }) {
  const [providers, setProviders] = useState([])
  const [models, setModels] = useState([])
  const [teams, setTeams] = useState([])

  useEffect(() => {
    const ac = new AbortController()
    fetchProviders(getKey(), ac.signal)
      .then((list) => {
        setProviders(list.map((p) => p.name))
        setModels([...new Set(list.flatMap((p) => p.models ?? []))].sort())
      })
      .catch(() => {})
    return () => ac.abort()
  }, [getKey])

  useEffect(() => {
    if (!isAdmin) return
    const ac = new AbortController()
    fetchTeams(getKey(), ac.signal)
      .then((list) => setTeams(list.map((t) => ({ id: t.id, name: t.name }))))
      .catch(() => {})
    return () => ac.abort()
  }, [getKey, isAdmin])

  return (
    <div className="logf">
      {isAdmin && (
        <Field label="Team" value={filters.team ?? ''} onChange={(v) => set('team', v)}>
          <option value="">All teams</option>
          {teams.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
        </Field>
      )}

      <Field label="Status" value={filters.status ?? ''} onChange={(v) => set('status', v)}>
        <option value="">Any status</option>
        {STATUSES.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
      </Field>

      <Field label="Provider" value={filters.provider ?? ''} onChange={(v) => set('provider', v)}>
        <option value="">Any provider</option>
        {providers.map((p) => <option key={p} value={p}>{p}</option>)}
      </Field>

      <Field label="Model" value={filters.model ?? ''} onChange={(v) => set('model', v)}>
        <option value="">Any model</option>
        {models.map((m) => <option key={m} value={m}>{m}</option>)}
      </Field>

      <Field label="Cache" value={filters.cache ?? ''} onChange={(v) => set('cache', v)}>
        <option value="">Any cache</option>
        <option value="hit">Hit</option>
        <option value="miss">Miss</option>
      </Field>

      <Field label="Time range" value={range} onChange={(v) => set('range', v)}>
        {RANGES.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
      </Field>

      <label className="logf-check">
        <input
          type="checkbox"
          checked={filters.fallback === 'true'}
          onChange={(e) => set('fallback', e.target.checked ? 'true' : '')}
        />
        <span>Fallback only</span>
      </label>

      {activeCount > 0 && (
        <div className="logf-active">
          <span className="num">{activeCount}</span> filter{activeCount === 1 ? '' : 's'} active
          <button type="button" className="logf-clear" onClick={clearAll}>Clear all</button>
        </div>
      )}
    </div>
  )
}
