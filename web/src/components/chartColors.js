// The one place a chart series gets a colour. Defined as token references so
// the palette lives in tokens.css and no component names a colour itself.
export const CHART_SERIES = ['var(--chart-series-1)', 'var(--chart-series-2)', 'var(--chart-series-3)']

// Overhead's three percentiles: p95 carries the accent because it is the
// number the project is measured against; p50 and p99 sit back.
export const OVERHEAD_SERIES = [
  { key: 'p50', label: 'p50', color: 'var(--chart-series-2)' },
  { key: 'p95', label: 'p95', color: 'var(--chart-series-1)' },
  { key: 'p99', label: 'p99', color: 'var(--chart-series-3)' },
]

// Recharts' defaults are wrong for this design; these are the shared overrides
// every chart spreads onto its axes and grid (see DESIGN.md "Charts").
export const CHART_MARGIN = { top: 8, right: 8, bottom: 0, left: 0 }

export const GRID_PROPS = {
  vertical: false,
  stroke: 'var(--border)',
  strokeDasharray: '3 3',
}

export const AXIS_PROPS = {
  axisLine: false,
  tickLine: false,
  tick: { fill: 'var(--text-muted)', fontSize: 11, fontFamily: 'var(--font-mono)' },
}

export const CURSOR_PROPS = { stroke: 'var(--border-strong)', strokeWidth: 1 }
