// The app's charts. Each takes the series shape its endpoint returns and
// applies the shared Recharts overrides from chartColors.js.
import {
  Bar, BarChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis,
} from 'recharts'
import {
  AXIS_PROPS, CHART_MARGIN, CHART_OVERFLOW, CHART_SERIES, CURSOR_PROPS, GRID_PROPS, OVERHEAD_SERIES,
} from './chartColors.js'
import ChartTooltip from './ChartTooltip.jsx'
import { EmptyState } from './states.jsx'
import { formatAxisTime, formatCount, formatMs, formatUSD } from '../utils/format.js'

const HEIGHT = 200

// Request volume per bucket. Bars because the value is a discrete count over a
// fixed step, not a continuously sampled quantity.
export function TrafficChart({ points, range }) {
  if (!points?.length) {
    return <EmptyState>No traffic in this window yet. Send a request from the Playground to see it here.</EmptyState>
  }
  const data = points.map((p) => ({ t: formatAxisTime(p.t, range), value: p.value }))

  return (
    <ResponsiveContainer width="100%" height={HEIGHT}>
      <BarChart data={data} margin={CHART_MARGIN} barCategoryGap="20%">
        <CartesianGrid {...GRID_PROPS} />
        <XAxis dataKey="t" {...AXIS_PROPS} minTickGap={32} />
        <YAxis {...AXIS_PROPS} width={44} domain={[0, 'auto']} tickFormatter={formatCount} />
        <Tooltip
          cursor={{ fill: 'transparent' }}
          content={<ChartTooltip formatValue={(v) => formatCount(v)} />}
        />
        <Bar dataKey="value" name="requests" fill="var(--chart-series-1)" radius={[2, 2, 0, 0]} maxBarSize={28} />
      </BarChart>
    </ResponsiveContainer>
  )
}

// Gateway overhead percentiles. Zero-based Y axis on purpose: the project's
// constraint is p95 under 10ms, and a zero floor is what makes that readable.
export function OverheadChart({ points, range }) {
  if (!points?.length) {
    return <EmptyState>No overhead samples in this window yet.</EmptyState>
  }
  const data = points.map((p) => ({
    t: formatAxisTime(p.t, range), p50: p.p50, p95: p.p95, p99: p.p99,
  }))

  return (
    <>
      <div className="chart-legend">
        {OVERHEAD_SERIES.map((s) => (
          <span key={s.key} className="chart-legend-item">
            <span className="chart-legend-swatch" style={{ background: s.color }} />
            {s.label}
          </span>
        ))}
      </div>
      <ResponsiveContainer width="100%" height={HEIGHT}>
        <LineChart data={data} margin={CHART_MARGIN}>
          <CartesianGrid {...GRID_PROPS} />
          <XAxis dataKey="t" {...AXIS_PROPS} minTickGap={32} />
          <YAxis {...AXIS_PROPS} width={44} domain={[0, 'auto']} tickFormatter={(v) => formatMs(v, 0)} />
          <Tooltip cursor={CURSOR_PROPS} content={<ChartTooltip formatValue={(v) => `${formatMs(v)} ms`} />} />
          {OVERHEAD_SERIES.map((s) => (
            <Line
              key={s.key}
              type="monotone"
              dataKey={s.key}
              name={s.label}
              stroke={s.color}
              strokeWidth={1.5}
              dot={false}
              activeDot={{ r: 3 }}
              connectNulls
            />
          ))}
        </LineChart>
      </ResponsiveContainer>
    </>
  )
}

// Cost over time, stacked by provider / model / team. Bars for the same reason
// TrafficChart uses them — cost per fixed bucket is a discrete total. Only the
// top three contributors get their own series; the rest fold into "Other" so
// the chart never exceeds the defined palette.
export function CostTrendChart({ data, range }) {
  if (!data?.series?.length) {
    return <EmptyState>No spend in this window yet.</EmptyState>
  }

  const totals = {}
  for (const p of data.series) {
    for (const [k, micros] of Object.entries(p.breakdown)) {
      totals[k] = (totals[k] ?? 0) + micros
    }
  }
  const ranked = Object.keys(totals).sort((a, b) => totals[b] - totals[a])
  const top = ranked.slice(0, 3)
  const hasOther = ranked.length > 3
  const seriesKeys = hasOther ? [...top, 'Other'] : top
  const colorFor = (k, i) => (k === 'Other' ? CHART_OVERFLOW : CHART_SERIES[i])
  const label = (k) => k || 'none'

  const rows = data.series.map((p) => {
    const row = { t: formatAxisTime(p.t, range) }
    let other = 0
    for (const [k, micros] of Object.entries(p.breakdown)) {
      if (top.includes(k)) row[k] = micros / 1e6
      else other += micros
    }
    if (hasOther) row.Other = other / 1e6
    return row
  })

  return (
    <>
      <div className="chart-legend">
        {seriesKeys.map((k, i) => (
          <span key={k} className="chart-legend-item">
            <span className="chart-legend-swatch" style={{ background: colorFor(k, i) }} />
            {label(k)}
          </span>
        ))}
      </div>
      <ResponsiveContainer width="100%" height={HEIGHT}>
        <BarChart data={rows} margin={CHART_MARGIN} barCategoryGap="20%">
          <CartesianGrid {...GRID_PROPS} />
          <XAxis dataKey="t" {...AXIS_PROPS} minTickGap={32} />
          <YAxis {...AXIS_PROPS} width={52} domain={[0, 'auto']} tickFormatter={formatUSD} />
          <Tooltip cursor={{ fill: 'transparent' }} content={<ChartTooltip formatValue={formatUSD} />} />
          {seriesKeys.map((k, i) => (
            <Bar
              key={k}
              dataKey={k}
              name={label(k)}
              stackId="cost"
              fill={colorFor(k, i)}
              maxBarSize={28}
              radius={i === seriesKeys.length - 1 ? [2, 2, 0, 0] : undefined}
            />
          ))}
        </BarChart>
      </ResponsiveContainer>
    </>
  )
}
