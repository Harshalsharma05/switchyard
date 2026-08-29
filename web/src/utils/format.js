// Display formatting. Every number the console shows goes through here so
// abbreviation, precision, and units stay consistent across screens.

// Counts abbreviate above 10,000 — 12.4k, 1.2M.
export function formatCount(n) {
  if (n === null || n === undefined) return '—'
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 10_000) return `${(n / 1000).toFixed(1)}k`
  return Math.round(n).toLocaleString()
}

export function formatMs(n, digits = 2) {
  if (n === null || n === undefined) return '—'
  return n.toFixed(digits)
}

export function formatUSD(n) {
  if (n === null || n === undefined) return '—'
  return `$${n.toFixed(2)}`
}

export function formatPercent(ratio, digits = 2) {
  if (ratio === null || ratio === undefined) return '—'
  return `${(ratio * 100).toFixed(digits)}%`
}

// Time axes format by range: HH:mm within a day, MMM d beyond it.
export function formatAxisTime(iso, range) {
  const d = new Date(iso)
  if (range === '7d') {
    return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  }
  return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false })
}

export function formatClock(iso) {
  return new Date(iso).toLocaleTimeString(undefined, {
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  })
}

// Long identifiers truncate with a middle ellipsis and never wrap.
export function middleTruncate(s, head = 8, tail = 6) {
  if (!s || s.length <= head + tail + 1) return s ?? ''
  return `${s.slice(0, head)}…${s.slice(-tail)}`
}
