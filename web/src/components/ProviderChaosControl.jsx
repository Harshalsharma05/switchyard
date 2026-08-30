// Per-provider fault injection (Step 7.5 / Part 2 Step 4.3). Sits on each
// provider card. Degrades to a plain note when the harness is unavailable
// rather than showing controls that would silently do nothing.
import { useState } from 'react'
import { useChaos } from '../hooks/useChaos.js'

const MODES = [
  { value: 'none', label: 'No fault' },
  { value: 'error', label: 'Force errors' },
  { value: 'latency', label: 'Add latency' },
  { value: 'rate_limit', label: 'Force 429s' },
  { value: 'drop', label: 'Drop connections' },
]

const LABEL = Object.fromEntries(MODES.map((m) => [m.value, m.label]))

export default function ProviderChaosControl({ provider }) {
  const chaos = useChaos()
  const [draft, setDraft] = useState(null) // {mode, latencyMs} while editing latency
  const [phase, setPhase] = useState('idle') // idle | pending | error

  if (!chaos) return null
  if (!chaos.available) {
    return (
      <p className="chaos-ctl-off">
        Fault injection is off — the gateway needs <code>SWITCHYARD_ENV=dev</code>.
      </p>
    )
  }

  const active = chaos.rules.find((r) => r.provider === provider && !r.model)

  // Rebuild the whole rule set, leaving every other rule (other providers,
  // model-scoped) untouched — the endpoint replaces the set wholesale.
  function build(mode, latencyMs) {
    const others = chaos.rules.filter((r) => !(r.provider === provider && !r.model))
    if (mode === 'none') return others
    const rule = { provider, mode }
    if (mode === 'latency') rule.latency_ms = latencyMs
    return [...others, rule]
  }

  async function apply(mode, latencyMs) {
    setPhase('pending')
    try {
      await chaos.applyRules(build(mode, latencyMs))
      setDraft(null)
      setPhase('idle')
    } catch {
      setPhase('error')
    }
  }

  function onSelect(mode) {
    if (mode === 'latency') {
      setDraft({ mode, latencyMs: active?.latency_ms ?? 200 })
    } else {
      setDraft(null)
      apply(mode)
    }
  }

  return (
    <div className="chaos-ctl">
      <label className="chaos-ctl-label">
        Fault
        <select
          className="chaos-ctl-select"
          value={draft?.mode ?? active?.mode ?? 'none'}
          disabled={phase === 'pending'}
          onChange={(e) => onSelect(e.target.value)}
        >
          {MODES.map((m) => (
            <option key={m.value} value={m.value}>{m.label}</option>
          ))}
        </select>
      </label>

      {draft?.mode === 'latency' && (
        <span className="chaos-ctl-latency">
          <input
            type="number"
            min="1"
            className="chaos-ctl-ms num"
            value={draft.latencyMs}
            onChange={(e) => setDraft({ ...draft, latencyMs: Number(e.target.value) })}
          />
          ms
          <button
            type="button"
            className="chaos-ctl-apply"
            disabled={phase === 'pending' || !(draft.latencyMs > 0)}
            onClick={() => apply('latency', draft.latencyMs)}
          >
            Apply
          </button>
        </span>
      )}

      {phase === 'pending' && <span className="chaos-ctl-note">Applying…</span>}
      {phase === 'error' && <span className="chaos-ctl-err">Could not apply — retry.</span>}
      {phase === 'idle' && active && (
        <span className="chaos-ctl-note">
          Active: {LABEL[active.mode] ?? active.mode}
          {active.mode === 'latency' && <> (<span className="num">{active.latency_ms}</span> ms)</>}
        </span>
      )}
    </div>
  )
}
