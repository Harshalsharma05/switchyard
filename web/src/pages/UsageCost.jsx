// Usage & Cost: the org-wide roll-up (Multi-user, Step 3.2) — total spend,
// requests, tokens, cache hit rate, and error rate aggregated across every
// project in the signed-in organisation — followed by month-to-date project
// spend against budget (Step 6.1), the Redis-vs-request-log reconciliation
// check (Step 6.1), the cost trend split by provider / model / project (Step
// 6.2), and the cache / routing / fallback attribution panels (Step 6.3).
// Project management moved to Settings in Step 6.4.
import { useCallback, useState } from 'react'
import { fetchProjects } from '../api/projects.js'
import { fetchAttribution, fetchCosts, fetchReconciliation } from '../api/usage.js'
import { fetchQualityFeedback } from '../api/quality.js'
import { fetchSummary } from '../api/summary.js'
import { Card, KpiCard } from '../components/primitives.jsx'
import { CostTrendChart } from '../components/charts.jsx'
import SpendCard from '../components/SpendCard.jsx'
import { EmptyState, ErrorState, Loading } from '../components/states.jsx'
import { useSession } from '../hooks/useSession.js'
import { useProjectScope } from '../hooks/useProjectScope.js'
import { usePolling } from '../hooks/usePolling.js'
import { formatCount, formatPercent, formatUSD, isZeroUSD } from '../utils/format.js'
import '../components/charts.css'
import './UsageCost.css'

// null and undefined both mean "no data" and must reach KpiCard as null so it
// renders an em dash, never a formatted zero (mirrors Overview's own `kpi`).
const kpi = (v, fmt) => (v == null ? null : fmt(v))

// A small inline check — a verification mark on the reconciliation line, not a
// status pill. 12px, healthy colour (Step 3, DESIGN.md: no emoji as status).
function CheckMark() {
  return (
    <svg className="recon-check" width="12" height="12" viewBox="0 0 24 24" fill="none"
      stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round"
      aria-hidden="true">
      <polyline points="20 6 9 17 4 12" />
    </svg>
  )
}

const RANGES = ['24h', '7d', '30d']

function Seg({ options, value, onChange, label, renderLabel = (o) => o }) {
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
          {renderLabel(o)}
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
        <CheckMark />
        Redis budget counters and the request log agree for {r.period}.
      </p>
    )
  }

  const off = r.teams.filter((t) => t.within_tolerance === false)
  return (
    <div className="recon recon-warn" role="status">
      {r.degraded && <div>Some projects’ Redis spend could not be read.</div>}
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

// One attribution headline figure. Sign convention: a negative `usd` is money
// saved (shown −, healthy colour), a positive one is money added (shown +,
// warn colour). A value that rounds to $0.00 renders neutral — no sign, no
// status colour, --text-muted — because at that size the sign is noise (Step 3).
function CostFigure({ usd }) {
  if (isZeroUSD(usd)) {
    return <span className="attr-value num attr-value-flat">{formatUSD(0)}</span>
  }
  const saved = usd < 0
  return (
    <span className={`attr-value num ${saved ? 'attr-value-ok' : 'attr-value-warn'}`}>
      {saved ? '−' : '+'}
      {formatUSD(Math.abs(usd))}
    </span>
  )
}

// Cache savings are priced from the real token counts on cache-hit rows, so
// the headline is money genuinely not spent rather than an estimate. A null
// panel means the gateway has no pricing table, not that savings were zero.
function CacheAttribution({ data }) {
  const c = data?.cache
  if (!c) return <span className="attr-context">The semantic cache is not enabled on this gateway.</span>

  const total = c.hits + c.misses
  // Hits served by a model with no configured price — the mock provider under a
  // load test, or a since-removed model — can't be priced, so a large hit count
  // can still sit against a near-zero saving. Say so rather than let the number
  // look broken (Step 3 investigation: 143 hits / $0.0001 saved was real, not a
  // bug — 140 of those hits were mock-provider traffic).
  const unpriced = c.hits - c.priced_hits
  return (
    <>
      <CostFigure usd={-c.saved_usd} />
      <span className="attr-context">
        {total === 0
          ? 'no cache lookups in this range'
          : `${c.hits} of ${total} lookups hit · ${(c.hit_rate * 100).toFixed(1)}% hit rate`}
        {unpriced > 0 && total > 0 && (
          <>
            {' · '}
            {unpriced} of {c.hits} served by a model with no price, not counted
          </>
        )}
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
      <CostFigure usd={-r.saved_usd} />
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
  const zero = isZeroUSD(net)
  return (
    <>
      <CostFigure usd={net} />
      <span className="attr-context">
        {zero ? 'no net effect' : net > 0 ? 'added by fallback' : 'saved by fallback'}
        {' · '}
        {formatUSD(extra)} added, {formatUSD(saved)} saved
      </span>
    </>
  )
}


// Response quality feedback (Phase 9.3). Two loops, surfaced not automated:
// near-threshold cache hits that scored badly say the similarity threshold may
// be too permissive; downgraded responses that scored badly are candidate
// classifier mislabels. Admin-only — the endpoint 403s a non-admin key.
function QualityLoop({ loop }) {
  if (!loop) return null
  const st = loop.stat
  return (
    <div className="attr">
      <span className="attr-title">{loop.reason.replace(/_/g, ' ')}</span>
      {st ? (
        <span className="attr-value num">
          {st.avg_score.toFixed(2)}<span className="ql-unit"> avg</span>
          {st.low_scored > 0 && <span className="ql-bad"> · {st.low_scored} low</span>}
        </span>
      ) : (
        <span className="attr-value num ql-none">—</span>
      )}
      <span className="attr-context">{loop.signal}</span>
    </div>
  )
}

function QualityFeedbackCard({ range }) {
  const load = useCallback((signal) => fetchQualityFeedback({ range, signal }), [range])
  const fb = usePolling(load, {
    interval: 30000,
    ignoreError: (e) => e.type === 'request_log_disabled' || e.status === 403,
  })
  const d = fb.data

  return (
    <Card title="Response quality" className="usage-placeholder">
      <p className="attr-caption">
        A sample of responses is scored 1–5 by an LLM judge. These two signals inform a
        deliberate adjustment to the cache threshold or the classifier — the gateway never retunes itself.
      </p>
      {fb.loading && !d ? (
        <Loading rows={2} />
      ) : fb.error && !d ? (
        <EmptyState>
          {fb.error.status === 403
            ? 'Quality feedback is admin-only.'
            : 'The request log is not configured, so quality feedback is unavailable.'}
        </EmptyState>
      ) : (
        <>
          <div className="attr-row">
            <QualityLoop loop={d?.cache} />
            <QualityLoop loop={d?.routing} />
          </div>
          {d?.examples?.length > 0 && (
            <div className="ql-examples">
              <span className="attr-title">Downgrades that scored low — candidate mislabels</span>
              <table className="table ql-table">
                <thead>
                  <tr><th>Request</th><th>Served model</th><th className="ta-r">Score</th><th>Classifier rationale</th></tr>
                </thead>
                <tbody>
                  {d.examples.map((e) => (
                    <tr key={e.request_id}>
                      <td className="num" title={e.request_id}>{e.request_id.slice(0, 10)}…</td>
                      <td className="num">{e.served_model}</td>
                      <td className="num ta-r ql-bad">{e.quality_score.toFixed(1)}</td>
                      <td className="num">{e.routing_reason || '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </Card>
  )
}

export default function UsageCost() {
  const { isSuperadmin } = useSession()
  const { projectId } = useProjectScope()
  const [range, setRange] = useState('7d')
  const [by, setBy] = useState('provider')

  // Cost trend's "by project" split stops meaning anything once the top bar
  // has already narrowed to one — drop back to a dimension that still varies.
  // Adjusted during render (React's pattern for "reset state when a prop
  // changes"), not in an effect, so the setState below doesn't cascade.
  const [byScopedTo, setByScopedTo] = useState(projectId)
  if (byScopedTo !== projectId) {
    setByScopedTo(projectId)
    if (projectId && by === 'team') setBy('provider')
  }

  // /admin/teams already scopes to the caller's own organisation for any
  // signed-in user (Phase 2) — a superadmin with no ?team= filter sees every
  // org instead, same as it always has. Neither case needs fetchMe's
  // single-project fallback any more.
  const loadSpend = useCallback((signal) => fetchProjects(signal), [])
  const spend = usePolling(loadSpend, { interval: 10000 })

  const loadSummary = useCallback((signal) => fetchSummary(range, projectId, signal), [range, projectId])
  const summary = usePolling(loadSummary, { interval: 15000 })

  const loadRecon = useCallback((signal) => fetchReconciliation({ team: projectId, signal }), [projectId])
  const recon = usePolling(loadRecon, { interval: 30000, enabled: isSuperadmin })

  const loadCosts = useCallback(
    (signal) => fetchCosts({ range, by, team: projectId, signal }),
    [range, by, projectId],
  )
  const costs = usePolling(loadCosts, {
    interval: 15000,
    ignoreError: (e) => e.type === 'request_log_disabled',
  })

  const loadAttribution = useCallback(
    (signal) => fetchAttribution({ range, team: projectId, signal }),
    [range, projectId],
  )
  const attribution = usePolling(loadAttribution, {
    interval: 30000,
    ignoreError: (e) => e.type === 'request_log_disabled',
  })

  // Selecting one project in the top bar collapses every comparison view on
  // this screen down to it (Step 3.3) — the spend-card grid becomes one card,
  // and there is nothing left to split the cost trend "by project" either.
  const allProjects = spend.data ?? []
  const projects = projectId ? allProjects.filter((p) => p.id === projectId) : allProjects
  // 'team' stays the wire value sent as `by` — the /admin/costs grouping key is
  // unchanged — but the segment reads "project" to match the renamed UI concept.
  const byOptions = projectId ? ['provider', 'model'] : ['provider', 'model', 'team']
  const byLabel = (o) => (o === 'team' ? 'project' : o)

  // Total spend is the sum of each project's own month-to-date figure, not a
  // second query — same source as the per-project cards below, so the two can
  // never disagree. A project with an unread Redis counter (spent_usd null)
  // is excluded from the sum rather than treated as zero.
  const knownSpend = projects.filter((p) => p.spent_usd != null)
  const totalSpendUSD = knownSpend.length ? knownSpend.reduce((sum, p) => sum + p.spent_usd, 0) : null
  const s = summary.data

  return (
    <>
      <h1 className="page-title">Usage &amp; cost</h1>

      {(summary.loading && !s) || (spend.loading && !spend.data) ? (
        <Loading rows={2} />
      ) : (summary.error && !s) && (spend.error && !spend.data) ? (
        <ErrorState
          message="Could not read the organisation summary."
          onRetry={() => { summary.refresh(); spend.refresh() }}
        />
      ) : (
        <div className="kpi-row">
          <KpiCard
            label="Total spend"
            value={kpi(totalSpendUSD, formatUSD)}
            context={projects.length ? `across ${projects.length} project${projects.length === 1 ? '' : 's'}` : undefined}
            empty={projects.length ? 'Some projects’ spend could not be read' : 'No projects yet'}
          />
          <KpiCard
            label="Requests"
            value={kpi(s?.requests?.total, formatCount)}
            context={`in the last ${range}`}
            empty="No traffic in this window"
          />
          <KpiCard
            label="Tokens"
            value={kpi(s?.tokens?.total, formatCount)}
            context={`in the last ${range}`}
            empty="No traffic in this window"
          />
          <KpiCard
            label="Cache hit rate"
            value={s?.cache?.enabled ? kpi(s.cache.hit_rate, formatPercent) : null}
            empty="Not yet enabled"
          />
          <KpiCard
            label="Error rate"
            value={kpi(s?.requests?.error_rate, formatPercent)}
            context="5xx responses only"
            empty="No traffic in this window"
          />
        </div>
      )}

      {spend.loading && !spend.data ? (
        <Loading rows={2} />
      ) : spend.error && !spend.data ? (
        <ErrorState message="Could not read project spend." onRetry={spend.refresh} />
      ) : projects.length === 0 ? (
        <EmptyState>
          {projectId ? 'This project could not be found.' : 'This organisation has no projects yet.'}
        </EmptyState>
      ) : (
        <>
          <div className="usage-cards">
            {projects.map((t) => (
              <SpendCard
                key={t.id}
                name={t.name}
                spentUSD={t.spent_usd}
                budgetUSD={t.monthly_budget_usd}
                utilization={t.budget_utilization}
              />
            ))}
          </div>
          {isSuperadmin && <ReconStrip state={recon} />}
        </>
      )}

      <Card
        title="Cost trend"
        action={
          <div className="usage-controls">
            <Seg options={byOptions} value={by} onChange={setBy} label="Split by" renderLabel={byLabel} />
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

      {isSuperadmin && <QualityFeedbackCard range={range} />}
    </>
  )
}
