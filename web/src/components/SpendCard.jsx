// One team's month-to-date spend against its budget (Step 6.1). A KPI card with
// a thin progress bar beneath, coloured by threshold: accent under 80%, warn at
// 80%, error at 100%.
import './SpendCard.css'

function tone(pct) {
  if (pct == null) return 'info'
  if (pct >= 1) return 'error'
  if (pct >= 0.8) return 'warn'
  return 'ok'
}

export default function SpendCard({ name, spentUSD, budgetUSD, utilization }) {
  const known = spentUSD != null
  const pct =
    utilization != null ? utilization : budgetUSD > 0 && known ? spentUSD / budgetUSD : null
  const t = tone(pct)
  const width = pct == null ? 0 : Math.min(100, pct * 100)

  return (
    <div className="spend">
      <span className="spend-name">{name}</span>
      <span className={`spend-value num ${known ? '' : 'spend-value-missing'}`}>
        {known ? `$${spentUSD.toFixed(2)}` : '—'}
      </span>
      <span className="spend-context">
        {!known
          ? 'spend could not be read'
          : budgetUSD > 0
            ? `of $${budgetUSD.toFixed(2)} · ${(pct * 100).toFixed(0)}% used`
            : 'no monthly budget set'}
        {t === 'warn' && <span className="spend-badge spend-badge-warn">Near limit</span>}
        {t === 'error' && <span className="spend-badge spend-badge-error">Over budget</span>}
      </span>
      <span className="spend-track" aria-hidden="true">
        <span className={`spend-fill spend-fill-${t}`} style={{ width: `${width}%` }} />
      </span>
    </div>
  )
}
