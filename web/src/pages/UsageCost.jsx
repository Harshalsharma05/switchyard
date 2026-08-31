// Usage & Cost: month-to-date team spend against budget (Step 6.1), the
// Redis-vs-request-log reconciliation check (Step 6.1), and the cost trend
// split by provider / model / team (Step 6.2). Attribution panels and team
// management arrive in Steps 6.3 and 6.4.
import { useCallback, useState } from 'react'
import { fetchMe } from '../api/session.js'
import { fetchTeams } from '../api/teams.js'
import { fetchAttribution, fetchCosts, fetchReconciliation } from '../api/usage.js'
import { Card } from '../components/primitives.jsx'
import { CostTrendChart } from '../components/charts.jsx'
import SpendCard from '../components/SpendCard.jsx'
import TeamTable from '../components/TeamTable.jsx'
import { EmptyState, ErrorState, Loading } from '../components/states.jsx'
import { useAuth } from '../hooks/useAuth.js'
import { usePolling } from '../hooks/usePolling.js'
import { formatUSD } from '../utils/format.js'
import '../components/charts.css'
import './UsageCost.css'

const RANGES = ['24h', '7d', '30d']

function Seg({ options, value, onChange, label }) {
  return (
    <div className="seg" role="group" aria-label={label}>
      {options.map((o) => (
        <button
          key={o}
          type="button"
          className={`seg-btn num ${o === value ? 'active' : ''}`}
          aria-pressed={o === value}
          onClick={() => onChange(o)}
        >
          {o}
        </button>
      ))}
    </div>
  )
}

// The reconciliation endpoint is admin-only; a discrepancy is surfaced, never
// hidden (Step 6.1).
function ReconStrip({ state }) {
  const r = state.data
  if (!r) return null

  if (r.reconciled && !r.degraded) {
    return (
      <p className="recon recon-ok" role="status">
        Redis budget counters and the request log agree for {r.period}.
      </p>
    )
  }

  const off = r.teams.filter((t) => t.within_tolerance === false)
  return (
    <div className="recon recon-warn" role="status">
      {r.degraded && <div>Some teams’ Redis spend could not be read.</div>}
      {off.length > 0 && (
        <>
          <div>Redis and the request log disagree beyond tolerance for {r.period}:</div>
          <ul className="recon-list">
            {off.map((t) => (
              <li key={t.team_id} className="num">
                {t.team_id}: Redis ${t.redis_usd?.toFixed(2)} vs log ${t.log_usd.toFixed(2)}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  )
}

// One attribution panel. `state` is a usePolling result; `render` turns its
// data into the panel body, or the panel shows its own empty state.
function AttributionPanel({ title, state, empty, render }) {
  let body
  if (state.loading && !state.data) body = <Loading rows={2} />
  else if (state.error && !state.data) body = <EmptyState>{empty}</EmptyState>
  else body = render(state.data)
  return (
    <div className="attr">
      <span className="attr-title">{title}</span>
      {body}
    </div>
  )
}

// Cache savings are priced from the real token counts on cache-hit rows, so
// the headline is money genuinely not spent rather than an estimate. A null
// panel means the gateway has no pricing table, not that savings were zero.
function CacheAttribution({ data }) {
  const c = data?.cache
  if (!c) return <span className="attr-context">The semantic cache is not enabled on this gateway.</span>

  const total = c.hits + c.misses
  return (
    <>
      <span className={`attr-value num ${c.saved_micros > 0 ? 'attr-value-ok' : ''}`}>
        {c.saved_micros > 0 ? `−${formatUSD(c.saved_usd)}` : formatUSD(0)}
      </span>
      <span className="attr-context">
        {total === 0
          ? 'no cache lookups in this range'
          : `${c.hits} of ${total} lookups hit · ${(c.hit_rate * 100).toFixed(1)}% hit rate`}
      </span>
    </>
  )
}

// Routing savings sum per-request deltas computed at decision time against
// real token counts, so this is money genuinely not spent. Deliberately not a
// projection over unrouted traffic — routing takes credit only for requests it
// actually decided.
function RoutingAttribution({ data }) {
  const r = data?.routing
  if (!r) return <span className="attr-context">Cost-aware routing is not enabled on this gateway.</span>

  return (
    <>
      <span className={`attr-value num ${r.saved_micros > 0 ? 'attr-value-ok' : ''}`}>
        {r.saved_micros > 0 ? `−${formatUSD(r.saved_usd)}` : formatUSD(0)}
      </span>
      <span className="attr-context">
        {r.routed === 0
          ? 'no routed requests in this range'
          : `${r.downgraded} of ${r.routed} routed down · ${(r.downgrade_rate * 100).toFixed(1)}% downgraded`}
      </span>
    </>
  )
}

function FallbackAttribution({ data }) {
  const { net_usd: net, extra_usd: extra, saved_usd: saved } = data.fallback
  const tone = net > 0 ? 'warn' : net < 0 ? 'ok' : 'flat'
  const headline =
    net > 0 ? `+${formatUSD(net)}` : net < 0 ? `−${formatUSD(-net)}` : formatUSD(0)
  return (
    <>
      <span className={`attr-value num attr-value-${tone}`}>{headline}</span>
      <span className="attr-context">
        {net > 0 ? 'added by fallback' : net < 0 ? 'saved by fallback' : 'no net effect'}
        {' · '}
        {formatUSD(extra)} added, {formatUSD(saved)} saved
      </span>
    </>
  )
}

export default function UsageCost() {
  const { getKey, isAdmin } = useAuth()
  const [range, setRange] = useState('7d')
  const [by, setBy] = useState('provider')

  const loadSpend = useCallback(
    (signal) => (isAdmin ? fetchTeams(getKey(), signal) : fetchMe(getKey(), signal)),
    [getKey, isAdmin],
  )
  const spend = usePolling(loadSpend, { interval: 10000 })

  const loadRecon = useCallback((signal) => fetchReconciliation(getKey(), signal), [getKey])
  const recon = usePolling(loadRecon, { interval: 30000, enabled: isAdmin })

  const loadCosts = useCallback(
    (signal) => fetchCosts(getKey(), { range, by, signal }),
    [getKey, range, by],
  )
  const costs = usePolling(loadCosts, {
    interval: 15000,
    ignoreError: (e) => e.type === 'request_log_disabled',
  })

  const loadAttribution = useCallback(
    (signal) => fetchAttribution(getKey(), { range, signal }),
    [getKey, range],
  )
  const attribution = usePolling(loadAttribution, {
    interval: 30000,
    ignoreError: (e) => e.type === 'request_log_disabled',
  })

  const teams = isAdmin ? (spend.data ?? []) : spend.data ? [spend.data] : []
  const byOptions = isAdmin ? ['provider', 'model', 'team'] : ['provider', 'model']

  return (
    <>
      <h1 className="page-title">Usage &amp; cost</h1>

      {spend.loading && !spend.data ? (
        <Loading rows={2} />
      ) : spend.error && !spend.data ? (
        <ErrorState message="Could not read team spend." onRetry={spend.refresh} />
      ) : (
        <>
          <div className="usage-cards">
            {teams.map((t) => (
              <SpendCard
                key={t.id}
                name={t.name}
                spentUSD={t.spent_usd}
                budgetUSD={t.monthly_budget_usd}
                utilization={t.budget_utilization}
              />
            ))}
          </div>
          {isAdmin && <ReconStrip state={recon} />}
        </>
      )}

      <Card
        title="Cost trend"
        action={
          <div className="usage-controls">
            <Seg options={byOptions} value={by} onChange={setBy} label="Split by" />
            <Seg options={RANGES} value={range} onChange={setRange} label="Time range" />
          </div>
        }
      >
        {costs.loading && !costs.data ? (
          <Loading rows={5} />
        ) : costs.error && !costs.data ? (
          costs.error.type === 'request_log_disabled' ? (
            <EmptyState>
              The request log is not configured on this gateway, so there is no
              cost history to chart.
            </EmptyState>
          ) : (
            <ErrorState message="Could not load the cost trend." onRetry={costs.refresh} />
          )
        ) : (
          <CostTrendChart data={costs.data} range={range} />
        )}
      </Card>

      <Card title="Cost attribution" className="usage-placeholder">
        <p className="attr-caption">What the cache, routing, and resilience cost or saved over the selected range.</p>
        <div className="attr-row">
          <AttributionPanel
            title="Saved by cache"
            state={attribution}
            empty="The request log is not configured, so cache savings cannot be attributed."
            render={(data) => <CacheAttribution data={data} />}
          />
          <AttributionPanel
            title="Saved by routing"
            state={attribution}
            empty="The request log is not configured, so routing savings cannot be attributed."
            render={(data) => <RoutingAttribution data={data} />}
          />
          <AttributionPanel
            title="Shifted by fallback"
            state={attribution}
            empty="The request log is not configured, so fallback cost cannot be attributed."
            render={(data) => <FallbackAttribution data={data} />}
          />
        </div>
      </Card>

      {isAdmin && (
        <Card title="Team management" className="usage-placeholder">
          {spend.loading && !spend.data ? (
            <Loading rows={3} />
          ) : spend.error && !spend.data ? (
            <ErrorState message="Could not load teams." onRetry={spend.refresh} />
          ) : (
            <TeamTable teams={teams} getKey={getKey} onChanged={spend.refresh} />
          )}
        </Card>
      )}
    </>
  )
}
