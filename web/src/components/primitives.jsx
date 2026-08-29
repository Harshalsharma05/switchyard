// The shared UI vocabulary from DESIGN.md: card, KPI card, and status pill.
// Every screen composes these rather than restyling a div.
import './primitives.css'

export function Card({ title, action, children, className = '' }) {
  return (
    <section className={`card ${className}`}>
      {(title || action) && (
        <header className="card-head">
          <h2 className="card-title">{title}</h2>
          {action}
        </header>
      )}
      {children}
    </section>
  )
}

// KpiCard renders `—` with a reason beneath when value is null, never a zero
// standing in for missing data.
export function KpiCard({ label, value, unit, context, empty }) {
  const missing = value === null || value === undefined
  return (
    <div className="kpi">
      <span className="kpi-label">{label}</span>
      <span className={`kpi-value num ${missing ? 'kpi-value-missing' : ''}`}>
        {missing ? '—' : value}
        {!missing && unit && <span className="kpi-unit">{unit}</span>}
      </span>
      <span className="kpi-context">{missing ? (empty ?? 'No data yet') : context}</span>
    </div>
  )
}

// Status is never colour alone — the dot always has a word beside it.
const TONE = {
  healthy: 'healthy', closed: 'healthy', ok: 'healthy',
  degraded: 'warn', half_open: 'warn',
  down: 'error', open: 'error',
}

export function StatusPill({ status, label }) {
  const tone = TONE[status] ?? 'info'
  return (
    <span className={`pill-status pill-${tone}`}>
      <span className="pill-dot" />
      {label ?? status.replace('_', '-')}
    </span>
  )
}

// httpTone maps a status code onto the same three-colour grammar: 2xx healthy,
// 429/402 warn (the system working as designed), everything else error.
export function StatusCode({ code }) {
  const tone = code < 300 ? 'healthy' : code === 429 || code === 402 ? 'warn' : 'error'
  return (
    <span className={`pill-status pill-${tone}`}>
      <span className="pill-dot" />
      <span className="num">{code}</span>
    </span>
  )
}
