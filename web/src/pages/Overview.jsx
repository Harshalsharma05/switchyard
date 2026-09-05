// Overview: KPI row, traffic and overhead charts, live provider health and
// breaker state, and the newest request-log rows. Everything on this screen
// updates on a poll (Step 2.5) — nothing here needs a manual refresh.
import { useCallback } from 'react'
import { fetchProviderHealth } from '../api/health.js'
import { fetchRequests } from '../api/requests.js'
import { fetchSummary } from '../api/summary.js'
import { Card, KpiCard } from '../components/primitives.jsx'
import { OverheadChart, QualityChart, TrafficChart } from '../components/charts.jsx'
import ProviderHealthPanel from '../components/ProviderHealthPanel.jsx'
import BreakerPanel from '../components/BreakerPanel.jsx'
import LiveFeed from '../components/LiveFeed.jsx'
import { EmptyState, ErrorState, Loading } from '../components/states.jsx'
import { useAuth } from '../hooks/useAuth.js'
import { usePolling } from '../hooks/usePolling.js'
import { useTimeRange } from '../hooks/useTimeRange.js'
import { formatCount, formatMs, formatPercent, formatUSD } from '../utils/format.js'
import '../components/charts.css'
import './Overview.css'

// Grafana runs alongside the gateway in compose; the base URL is an env
// override (VITE_GRAFANA_URL) so a different deployment needs no code change.
// switchyard-performance is the provisioned dashboard's stable UID
// (deploy/grafana/dashboards/performance.json) — the one that mirrors this
// screen's traffic and overhead charts. The link carries the current range
// through, so clicking it lands on the same window in the real stack: the
// point of the affordance is that both read the same Prometheus.
const GRAFANA_BASE = import.meta.env.VITE_GRAFANA_URL ?? 'http://localhost:3000'

function grafanaURL(range) {
  return `${GRAFANA_BASE}/d/switchyard-performance?from=now-${range}&to=now&refresh=10s`
}

// null and undefined both mean "no data" and must reach KpiCard as null so it
// renders an em dash, never a formatted zero.
const kpi = (v, fmt) => (v == null ? null : fmt(v))

export default function Overview() {
  const { getKey, isAdmin } = useAuth()
  const { range } = useTimeRange()

  const loadSummary = useCallback((signal) => fetchSummary(getKey(), range, signal), [getKey, range])
  const loadHealth = useCallback((signal) => fetchProviderHealth(getKey(), signal), [getKey])
  const loadFeed = useCallback((signal) => fetchRequests(getKey(), { limit: 25, signal }), [getKey])

  const summary = usePolling(loadSummary, { interval: 5000 })
  const health = usePolling(loadHealth, { interval: 5000 })
  const feed = usePolling(loadFeed, { interval: 3000 })

  const s = summary.data

  return (
    <>
      <h1 className="page-title">Overview</h1>

      {s?.degraded && (
        <p className="degraded" role="status">
          Some metrics are unavailable — Prometheus did not answer. Live provider
          health and the request feed are unaffected.
        </p>
      )}

      {summary.loading && !s ? (
        <Loading rows={5} />
      ) : summary.error && !s ? (
        <ErrorState
          message="Could not load the summary. The gateway may be unreachable."
          onRetry={summary.refresh}
        />
      ) : (
        <div className="kpi-row">
          <KpiCard
            label="Requests"
            value={kpi(s?.requests?.total, formatCount)}
            context={`in the last ${range}`}
            empty="No traffic in this window"
          />
          <KpiCard
            label="Overhead p95"
            value={kpi(s?.overhead_ms?.p95, formatMs)}
            unit="ms"
            context="target is under 10 ms"
            empty="No samples in this window"
          />
          <KpiCard
            label="Error rate"
            value={kpi(s?.requests?.error_rate, formatPercent)}
            context="5xx responses only"
            empty="No traffic in this window"
          />
          <KpiCard
            label="Cache hit rate"
            value={s?.cache?.enabled ? kpi(s.cache.hit_rate, formatPercent) : null}
            empty="Not yet enabled"
          />
          <KpiCard
            label="Cost"
            value={kpi(s?.cost?.total_usd, formatUSD)}
            context={`in the last ${range}`}
            empty="No spend in this window"
          />
          <KpiCard
            label="Avg quality"
            value={s?.quality?.enabled ? kpi(s.quality.avg_score, (v) => v.toFixed(2)) : null}
            unit="/ 5"
            context={
              s?.quality?.scored != null
                ? `${formatCount(s.quality.scored)} scored in the last ${range}`
                : 'async judge score'
            }
            empty="Not yet enabled"
          />
        </div>
      )}

      <div className="split">
        <div className="split-main">
          <Card
            title="Request volume"
            action={
              <a className="card-link" href={grafanaURL(range)} target="_blank" rel="noreferrer">
                Open in Grafana ↗
              </a>
            }
          >
            <TrafficChart points={s?.series?.traffic} range={range} />
          </Card>

          <Card title="Gateway overhead">
            <OverheadChart points={s?.series?.overhead} range={range} />
          </Card>

          <Card title="Response quality">
            <QualityChart points={s?.series?.quality} range={range} />
          </Card>
        </div>

        <div className="split-side">
          <Card title="Provider health">
            {health.loading && !health.data ? (
              <Loading rows={3} />
            ) : health.error && !health.data ? (
              <ErrorState message="Could not read provider health." onRetry={health.refresh} />
            ) : (
              <ProviderHealthPanel providers={health.data} />
            )}
          </Card>

          <Card title="Circuit breakers">
            {health.loading && !health.data ? (
              <Loading rows={3} />
            ) : (
              <BreakerPanel providers={health.data} />
            )}
          </Card>
        </div>
      </div>

      <Card title="Live requests" className="feed-card">
        {feed.loading && !feed.data ? (
          <Loading rows={4} />
        ) : feed.error && !feed.data ? (
          feed.error.type === 'request_log_disabled' ? (
            <EmptyState>
              The request log is not configured on this gateway, so there is
              nothing to feed this table.
            </EmptyState>
          ) : (
            <ErrorState message="Could not read the request log." onRetry={feed.refresh} />
          )
        ) : (
          <LiveFeed rows={feed.data?.requests} showTeam={isAdmin} />
        )}
      </Card>
    </>
  )
}
