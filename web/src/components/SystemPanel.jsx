// System info (Settings §4): version, uptime, config fingerprint, live
// dependency status, the provider list, and the config-reload control. Backed by
// GET /admin/system + POST /admin/reload.
import { useCallback, useState } from 'react'
import { fetchSystem, reloadConfig } from '../api/system.js'
import { StatusPill } from './primitives.jsx'
import { EmptyState, ErrorState, Loading } from './states.jsx'
import { useAuth } from '../hooks/useAuth.js'
import { usePolling } from '../hooks/usePolling.js'

function uptime(seconds) {
  if (seconds == null) return '—'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const parts = []
  if (d) parts.push(`${d}d`)
  if (h || d) parts.push(`${h}h`)
  parts.push(`${m}m`)
  return parts.join(' ')
}

// "up" → healthy, "down" → error, "degraded" → warn, "not_configured" → info.
// StatusPill puts the literal word beside the dot, so it is never colour alone.
const DEP_TONE = { up: 'healthy', down: 'down', degraded: 'warn', not_configured: 'info' }
const DEP_LABEL = { up: 'up', down: 'down', degraded: 'degraded', not_configured: 'not configured' }

function ReloadControl({ getKey, onReloaded }) {
  const [phase, setPhase] = useState('idle') // idle | confirm | pending
  const [result, setResult] = useState(null)
  const [error, setError] = useState(null)

  const run = async () => {
    setPhase('pending')
    setError(null)
    try {
      const r = await reloadConfig(getKey())
      setResult(r)
      setPhase('idle')
      onReloaded?.()
    } catch (e) {
      setError(e.message || 'reload failed')
      setPhase('idle')
    }
  }

  return (
    <div className="sys-reload">
      {phase === 'confirm' ? (
        <span className="tt-confirm">
          Reload config from disk?
          <button type="button" className="tt-btn tt-btn-sm tt-btn-primary" onClick={run}>Reload</button>
          <button type="button" className="tt-btn tt-btn-sm" onClick={() => setPhase('idle')}>Cancel</button>
        </span>
      ) : (
        <button type="button" className="tt-btn" onClick={() => setPhase('confirm')} disabled={phase === 'pending'}>
          {phase === 'pending' ? 'Reloading…' : 'Reload config'}
        </button>
      )}
      {result && !error && (
        <span className="sys-reload-ok num">reloaded · {result.providers} providers · {result.teams} teams</span>
      )}
      {error && <span className="sys-reload-err">{error}</span>}
      <p className="sys-note">
        Reload re-reads <code className="num">configs/providers.yaml</code> only. Teams live
        in Postgres, so a key rotation or limit edit is never reverted by a reload — the
        audit log records every reload.
      </p>
    </div>
  )
}

export default function SystemPanel() {
  const { getKey } = useAuth()
  const load = useCallback((signal) => fetchSystem(getKey(), signal), [getKey])
  const sys = usePolling(load, { interval: 15000 })
  const d = sys.data

  if (sys.loading && !d) return <Loading rows={4} />
  if (sys.error && !d) {
    return sys.error.type === 'system_info_unavailable' ? (
      <EmptyState>This gateway does not expose system info.</EmptyState>
    ) : (
      <ErrorState message="Could not read system info." onRetry={sys.refresh} />
    )
  }

  const deps = d.dependencies ?? {}

  return (
    <div className="sys">
      <dl className="sys-list">
        <dt>Version</dt>
        <dd className="num">{d.version}</dd>
        <dt>Uptime</dt>
        <dd className="num">{uptime(d.uptime_seconds)}</dd>
        <dt>Config hash</dt>
        <dd className="num" title={d.config_hash}>{d.config_hash?.slice(0, 12) || '—'}</dd>
      </dl>

      <div className="sys-deps">
        {Object.entries(deps).map(([name, statev]) => (
          <span key={name} className="sys-dep">
            <span className="sys-dep-name">{name}</span>
            <StatusPill status={DEP_TONE[statev] ?? 'info'} label={DEP_LABEL[statev] ?? statev} />
          </span>
        ))}
      </div>

      <div className="sys-providers">
        <span className="sys-sub">Providers</span>
        <ul className="sys-provider-list">
          {(d.providers ?? []).map((p) => (
            <li key={p.name} className="num">
              {p.name} <span className="audit-muted">· {p.type} · {p.models?.length ?? 0} models</span>
            </li>
          ))}
        </ul>
      </div>

      <ReloadControl getKey={getKey} onReloaded={sys.refresh} />
    </div>
  )
}
