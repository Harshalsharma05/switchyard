// Recharts' default tooltip matches nothing else in this app, so every chart
// passes this one instead.
import './ChartTooltip.css'

export default function ChartTooltip({ active, payload, label, formatValue }) {
  if (!active || !payload?.length) return null
  return (
    <div className="charttip">
      <div className="charttip-label">{label}</div>
      {payload.map((p) => (
        <div key={p.dataKey} className="charttip-row">
          <span className="charttip-swatch" style={{ background: p.color }} />
          <span className="charttip-name">{p.name}</span>
          <span className="charttip-value num">
            {formatValue ? formatValue(p.value, p.dataKey) : p.value}
          </span>
        </div>
      ))}
    </div>
  )
}
