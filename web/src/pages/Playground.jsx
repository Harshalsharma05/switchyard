// Playground — a live prompt against POST /v1/chat/completions, rendering the
// response as it streams, a metadata panel, 429 / 402 / 503 responses as
// first-class structured output, and this session's prompt history.
import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { sendChat } from '../api/chat.js'
import { ApiError } from '../api/client.js'
import { awaitRequestRow } from '../api/requests.js'
import { fetchMe } from '../api/session.js'
import { Card } from '../components/primitives.jsx'
import { EmptyState } from '../components/states.jsx'
import { useAuth } from '../hooks/useAuth.js'
import { getHistory, pushHistory, subscribeHistory } from '../hooks/playgroundHistory.js'
import { formatClock, formatMicroDollars, formatMs, formatPercent, formatUSD } from '../utils/format.js'
import './Playground.css'

// Header metadata renders immediately; latency, tokens, and cost arrive once the
// async request-log row lands — "settling…" until then, "—" if it never does.
function MetadataPanel({ meta, row, rowPending }) {
  const later = rowPending ? 'settling…' : '—'
  return (
    <dl className="pg-meta">
      <dt>Provider</dt>
      <dd>{meta.provider ?? '—'}</dd>
      <dt>Requested model</dt>
      <dd>{meta.requestedModel ?? '—'}</dd>
      <dt>Served model</dt>
      <dd>
        {meta.servedModel ?? '—'}
        {meta.fallback && <span className="pg-fallback">fallback</span>}
      </dd>
      <dt>Routing</dt>
      <dd>
        {meta.routeTier == null
          ? <span className="pg-muted">not routed — model named by caller</span>
          : <>
              <span className="pg-fallback">{meta.routeTier} tier</span>
              {meta.routeReason && <span className="pg-unit"> · {meta.routeReason}</span>}
            </>}
      </dd>
      <dt>Gateway overhead</dt>
      <dd>{formatMs(row?.overhead_ms ?? meta.overheadMs)}<span className="pg-unit"> ms</span></dd>
      <dt>Latency</dt>
      <dd>{row ? <>{formatMs(row.latency_ms)}<span className="pg-unit"> ms</span></> : later}</dd>
      <dt>Tokens in / out</dt>
      <dd>{row ? `${row.input_tokens} / ${row.output_tokens}` : later}</dd>
      <dt>Cost</dt>
      <dd>{row ? formatMicroDollars(row.cost_micros) : later}</dd>
      <dt>Cache</dt>
      <dd>
        {meta.cache == null
          ? <span className="pg-muted">not enabled</span>
          : meta.cache === 'miss'
            ? <span className="pg-muted">miss</span>
            : <span className="pg-fallback">{meta.cache} hit</span>}
        {meta.embedMs != null && <span className="pg-unit"> · {formatMs(meta.embedMs)} ms embed</span>}
      </dd>
    </dl>
  )
}

const TITLES = {
  rate_limit_exceeded: 'Rate limit exceeded',
  priority_shed: 'Shed — batch priority',
  budget_exceeded: 'Budget exhausted',
  chain_exhausted: 'Every provider exhausted',
  budget_unavailable: 'Budget check unavailable',
}

// 429 / 402 / 503 are the gateway working as designed, so each renders as a
// labelled block with its structured detail — never as a raw error string.
function ErrorPanel({ error, budget, partial }) {
  const tone = error.status === 429 || error.status === 402 ? 'warn' : 'error'
  const attempts = error.detail?.switchyard_attempts
  const title = partial ? 'Stream interrupted' : TITLES[error.type] ?? 'Request failed'

  return (
    <div className={`pg-err pg-err-${tone}`} role="alert">
      <div className="pg-err-head">
        <span className="num">{error.status || '—'}</span>
        <span>{title}</span>
      </div>
      <p className="pg-err-msg">{error.message}</p>

      {error.status === 429 && (
        <dl className="pg-err-grid">
          {error.retryAfter != null && (<><dt>Retry after</dt><dd>{error.retryAfter}s</dd></>)}
          {error.rateLimit && (
            <>
              <dt>Limit</dt><dd>{error.rateLimit.limit}/min</dd>
              <dt>Remaining</dt><dd>{error.rateLimit.remaining}</dd>
              <dt>Bucket full in</dt><dd>{error.rateLimit.reset}s</dd>
            </>
          )}
        </dl>
      )}

      {error.status === 402 && budget && (
        <dl className="pg-err-grid">
          <dt>Spent</dt><dd>{formatUSD(budget.spent_usd)}</dd>
          <dt>Monthly cap</dt><dd>{formatUSD(budget.monthly_budget_usd)}</dd>
          <dt>Utilisation</dt><dd>{formatPercent(budget.budget_utilization)}</dd>
        </dl>
      )}

      {attempts?.length > 0 && (
        <ul className="pg-err-attempts">
          {attempts.map((a, i) => (
            <li key={`${a.provider}-${a.model}-${i}`}>
              <span className="pg-att-cand">{a.provider} / {a.model}</span>
              <span className="pg-att-type">{a.type.replace(/_/g, ' ')}</span>
              <span className="pg-att-n num">{a.attempts}×</span>
              <span className="pg-att-msg">{a.message}</span>
            </li>
          ))}
        </ul>
      )}

      {partial && <p className="pg-err-partial">The response above stopped partway through.</p>}
    </div>
  )
}

export default function Playground() {
  const { me, getKey } = useAuth()
  const models = me?.allowed_models ?? []
  // What this gateway accepts in `model` beyond real model names — "auto" and
  // each routable tier. Empty when routing is off, so the selector simply does
  // not offer it; the tier names come from the API, never from here.
  const routingOptions = me?.routing_options ?? []

  const [prompt, setPrompt] = useState('')
  const [model, setModel] = useState(models[0] ?? '')
  const [streaming, setStreaming] = useState(true)
  const [output, setOutput] = useState('')
  const [error, setError] = useState(null)
  const [budget, setBudget] = useState(null)
  const [running, setRunning] = useState(false)
  const [meta, setMeta] = useState(null)
  const [row, setRow] = useState(null)
  const [rowPending, setRowPending] = useState(false)
  const runRef = useRef(null)
  const history = useSyncExternalStore(subscribeHistory, getHistory)

  // A run in flight — stream or the row poll that follows it — is abandoned when
  // the user leaves the screen, which aborts the fetch and stops generation.
  useEffect(() => () => runRef.current?.abort(), [])

  // opts lets the history list re-run an entry without waiting on the state
  // setters it also triggers for the visible controls.
  async function run(opts = {}) {
    const text = (opts.prompt ?? prompt).trim()
    const useModel = opts.model ?? model
    const useStream = opts.stream ?? streaming
    if (!text || running || !useModel) return

    runRef.current?.abort()
    const ac = new AbortController()
    runRef.current = ac

    setRunning(true)
    setError(null)
    setBudget(null)
    setOutput('')
    setMeta(null)
    setRow(null)
    setRowPending(false)
    pushHistory({ prompt: text, model: useModel, streaming: useStream })

    let captured = null
    try {
      await sendChat({
        key: getKey(),
        prompt: text,
        model: useModel,
        stream: useStream,
        signal: ac.signal,
        onMeta: (m) => { captured = m; setMeta(m) },
        onDelta: (d) => setOutput((prev) => prev + d),
      })
    } catch (err) {
      if (err.name === 'AbortError') { setRunning(false); return }
      const apiErr = err instanceof ApiError ? err : new ApiError(0, 'error', 'the request failed')
      setError(apiErr)
      setRunning(false)
      // The 402 body states the numbers as text; pull them as fields from a
      // fresh /admin/me so the panel can show cap and spend distinctly.
      if (apiErr.status === 402) {
        fetchMe(getKey(), ac.signal).then((identity) => {
          if (!ac.signal.aborted) setBudget(identity)
        }).catch(() => { /* fall back to the message text */ })
      }
      return
    }
    setRunning(false)

    if (captured?.requestId) {
      setRowPending(true)
      const r = await awaitRequestRow(getKey(), captured.requestId, { signal: ac.signal })
      if (!ac.signal.aborted) {
        setRow(r)
        setRowPending(false)
      }
    }
  }

  function onKeyDown(e) {
    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') run()
  }

  function rerun(entry) {
    setPrompt(entry.prompt)
    setModel(entry.model)
    setStreaming(entry.streaming)
    run({ prompt: entry.prompt, model: entry.model, stream: entry.streaming })
  }

  return (
    <>
      <h1 className="page-title">Playground</h1>

      <div className="pg">
        <div className="pg-main">
          <Card>
            <textarea
              className="pg-prompt"
              placeholder="Ask the gateway something…"
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              onKeyDown={onKeyDown}
              rows={5}
            />

            <div className="pg-controls">
              <label className="pg-field">
                <span>Model</span>
                <select
                  className="pg-select"
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                  disabled={models.length === 0 && routingOptions.length === 0}
                >
                  {models.length === 0 && routingOptions.length === 0 && (
                    <option value="">no models allowed</option>
                  )}
                  {routingOptions.length > 0 && (
                    <optgroup label="Cost-aware routing">
                      {routingOptions.map((o) => (
                        <option key={o} value={o}>
                          {o === 'auto' ? 'auto — classify, then choose a tier' : `${o} — pin this tier`}
                        </option>
                      ))}
                    </optgroup>
                  )}
                  {models.length > 0 && (
                    <optgroup label="Models">
                      {models.map((m) => (
                        <option key={m} value={m}>{m}</option>
                      ))}
                    </optgroup>
                  )}
                </select>
              </label>

              <label className="pg-toggle">
                <input
                  type="checkbox"
                  checked={streaming}
                  onChange={(e) => setStreaming(e.target.checked)}
                />
                <span>Stream</span>
              </label>

              <button
                type="button"
                className="pg-send"
                onClick={() => run()}
                disabled={running || !prompt.trim() || !model}
              >
                {running ? 'Sending…' : 'Send'}
              </button>
            </div>
          </Card>

          <Card title="Response">
            {output && <pre className="pg-response">{output}</pre>}
            {error ? (
              <ErrorPanel error={error} budget={budget} partial={!!output} />
            ) : !output && running ? (
              <p className="pg-waiting">Waiting for the first token…</p>
            ) : !output && !running ? (
              <EmptyState>Send a prompt to see the response stream in here.</EmptyState>
            ) : null}
          </Card>
        </div>

        <div className="pg-side">
          <Card title="Request metadata">
            {meta ? (
              <MetadataPanel meta={meta} row={row} rowPending={rowPending} />
            ) : (
              <EmptyState>Metadata for the latest request appears here after you send one.</EmptyState>
            )}
          </Card>

          <Card title="History">
            {history.length === 0 ? (
              <EmptyState>Prompts you send this session appear here to re-run.</EmptyState>
            ) : (
              <ul className="pg-hist">
                {history.map((h) => (
                  <li key={h.id}>
                    <button
                      type="button"
                      className="pg-hist-item"
                      onClick={() => rerun(h)}
                      disabled={running}
                    >
                      <span className="pg-hist-prompt">{h.prompt}</span>
                      <span className="pg-hist-meta num">{h.model} · {formatClock(h.at)}</span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </div>
      </div>
    </>
  )
}
