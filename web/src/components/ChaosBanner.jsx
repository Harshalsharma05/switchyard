// Persistent, unmissable warning shown on every screen while any chaos rule is
// active (DESIGN.md). Clearing confirms inline before it fires.
import { useState } from 'react'
import { useChaos } from '../hooks/useChaos.js'
import './ChaosBanner.css'

export default function ChaosBanner() {
  const chaos = useChaos()
  const [phase, setPhase] = useState('idle') // idle | confirming | pending | error

  const count = chaos?.rules?.length ?? 0
  if (!chaos || count === 0) return null

  async function clear() {
    setPhase('pending')
    try {
      await chaos.clearAll()
      setPhase('idle')
    } catch {
      setPhase('error')
    }
  }

  return (
    <div className="chaos-banner" role="alert" aria-live="polite">
      <span className="chaos-banner-text">
        Fault injection is active — {count} rule{count === 1 ? '' : 's'} forging
        provider failures on this gateway.
      </span>
      {phase === 'pending' ? (
        <span className="chaos-banner-note">Clearing…</span>
      ) : phase === 'confirming' ? (
        <span className="chaos-banner-confirm">
          Clear all chaos rules?
          <button type="button" className="chaos-banner-yes" onClick={clear}>Confirm</button>
          <button type="button" className="chaos-banner-no" onClick={() => setPhase('idle')}>Cancel</button>
        </span>
      ) : (
        <button type="button" className="chaos-banner-clear" onClick={() => setPhase('confirming')}>
          {phase === 'error' ? 'Clear failed — retry' : 'Clear all'}
        </button>
      )}
    </div>
  )
}
