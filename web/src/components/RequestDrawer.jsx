// Request Logs row-detail drawer (Step 5.3): the full stored record, what can
// be said about routing today, and a deep link to the request's Jaeger trace.
// A right-side drawer, not a modal — the list stays visible behind it.
import { useEffect, useRef } from 'react'
import { StatusCode } from './primitives.jsx'
import { formatDateTime, formatMicroDollars, formatMs } from '../utils/format.js'
import './RequestDrawer.css'

// Same env-override pattern as Overview's Grafana link; defaults to the compose
// Jaeger UI. /trace/{id} is Jaeger's own permalink shape.
const JAEGER_BASE = import.meta.env.VITE_JAEGER_URL ?? 'http://localhost:16686'
const jaegerTraceURL = (id) => `${JAEGER_BASE}/trace/${id}`

function Row({ label, children }) {
  return (
    <>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </>
  )
}

function RequestDetail({ r }) {
  const servedDifferently = r.requested_model && r.served_model && r.requested_model !== r.served_model

  return (
    <div className="rd">
      <section className="rd-sec">
        <h3 className="rd-sec-title">Request</h3>
        <dl className="rd-list">
          <Row label="ID"><span className="num rd-break">{r.id}</span></Row>
          <Row label="Time"><span className="num">{formatDateTime(r.timestamp)}</span></Row>
          <Row label="Team"><span className="num">{r.team_id}</span></Row>
          <Row label="Status"><StatusCode code={r.status_code} /></Row>
        </dl>
      </section>

      <section className="rd-sec">
        <h3 className="rd-sec-title">Routing</h3>
        <dl className="rd-list">
          <Row label="Requested model"><span className="num">{r.requested_model || '—'}</span></Row>
          <Row label="Served model"><span className="num">{r.served_model || '—'}</span></Row>
          <Row label="Provider"><span className="num">{r.provider || '—'}</span></Row>
          <Row label="Fallback">
            {r.fallback
              ? <span className="rd-flag num">{r.requested_model} → {r.served_model}</span>
              : <span className="rd-muted">no</span>}
          </Row>
          {servedDifferently && !r.fallback && (
            <Row label="Served differently">
              <span className="num">{r.requested_model} → {r.served_model}</span>
            </Row>
          )}
          <Row label="Cost-aware routing">
            {r.routing_tier
              ? <span className="rd-flag num">{r.routing_tier} tier</span>
              : <span className="rd-muted">not routed — model named by caller</span>}
          </Row>
          {r.routing_reason && (
            <Row label="Rationale"><span className="num rd-break">{r.routing_reason}</span></Row>
          )}
        </dl>
      </section>

      <section className="rd-sec">
        <h3 className="rd-sec-title">Tokens and cost</h3>
        <dl className="rd-list">
          <Row label="Input tokens"><span className="num">{r.input_tokens}</span></Row>
          <Row label="Output tokens"><span className="num">{r.output_tokens}</span></Row>
          <Row label="Cost"><span className="num">{formatMicroDollars(r.cost_micros)}</span></Row>
        </dl>
      </section>

      <section className="rd-sec">
        <h3 className="rd-sec-title">Latency</h3>
        <dl className="rd-list">
          <Row label="Total latency"><span className="num">{formatMs(r.latency_ms)}<span className="rd-unit"> ms</span></span></Row>
          <Row label="Gateway overhead"><span className="num">{formatMs(r.overhead_ms)}<span className="rd-unit"> ms</span></span></Row>
        </dl>
      </section>

      <section className="rd-sec">
        <h3 className="rd-sec-title">Quality and cache</h3>
        <dl className="rd-list">
          <Row label="Quality score">
            {r.quality_score == null
              ? <span className="rd-muted">not scored — sampling is deliberate, not every response</span>
              : <span className="num">{r.quality_score.toFixed(2)}<span className="rd-unit"> / 5</span></span>}
          </Row>
          {r.quality_sample_reason && (
            <Row label="Sampled because">
              <span className="num">{r.quality_sample_reason.replace(/_/g, ' ')}</span>
            </Row>
          )}
          <Row label="Cache">
            {r.cache_hit == null
              ? <span className="rd-muted">Not yet enabled</span>
              : <span className={r.cache_hit ? 'rd-flag' : 'rd-muted'}>{r.cache_hit ? 'hit' : 'miss'}</span>}
          </Row>
        </dl>
      </section>

      <section className="rd-sec">
        <h3 className="rd-sec-title">Trace</h3>
        {r.trace_id ? (
          <>
            <p className="rd-trace num rd-break">{r.trace_id}</p>
            <a className="rd-link" href={jaegerTraceURL(r.trace_id)} target="_blank" rel="noreferrer">
              Open trace in Jaeger ↗
            </a>
          </>
        ) : (
          <p className="rd-muted">No trace was recorded for this request.</p>
        )}
      </section>
    </div>
  )
}

export default function RequestDrawer({ row, onClose }) {
  const panelRef = useRef(null)

  // Focus moves into the drawer on open and back to the opener on close; Tab is
  // trapped inside and Escape closes — the drawer behaviour DESIGN.md requires.
  useEffect(() => {
    const opener = document.activeElement
    const panel = panelRef.current
    const items = () => [...panel.querySelectorAll('a[href],button,[tabindex]:not([tabindex="-1"])')]
    items()[0]?.focus()

    // The drawer is aria-modal; stop the list behind the scrim from scrolling.
    const prevOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'

    function onKey(e) {
      if (e.key === 'Escape') { onClose(); return }
      if (e.key !== 'Tab') return
      const f = items()
      if (!f.length) return
      const i = f.indexOf(document.activeElement)
      if (e.shiftKey && i <= 0) { e.preventDefault(); f[f.length - 1].focus() }
      else if (!e.shiftKey && i === f.length - 1) { e.preventDefault(); f[0].focus() }
    }
    panel.addEventListener('keydown', onKey)
    return () => {
      panel.removeEventListener('keydown', onKey)
      document.body.style.overflow = prevOverflow
      opener?.focus?.()
    }
  }, [onClose])

  return (
    <div className="rd-overlay">
      <div className="rd-scrim" onClick={onClose} />
      <aside className="rd-panel" ref={panelRef} role="dialog" aria-modal="true" aria-label="Request detail">
        <header className="rd-head">
          <span className="rd-head-title">Request detail</span>
          <button type="button" className="rd-close" onClick={onClose} aria-label="Close">✕</button>
        </header>
        <RequestDetail r={row} />
      </aside>
    </div>
  )
}
