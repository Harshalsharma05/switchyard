// Collects a team API key for the screens that exercise the gateway port.
//
// Signing in identifies a person; it does not authorise a completion. :8080
// authenticates teams and ignores the session cookie entirely, so Playground
// and the load simulator need a team key even when the operator is signed in.
// Stating that inline is better than a 401 the user has to interpret.
import { useState } from 'react'
import { useGatewayKey } from '../hooks/useGatewayKey.js'

export default function GatewayKeyField({ what }) {
  const { key, setKey, hasKey } = useGatewayKey()
  const [draft, setDraft] = useState(key)

  if (hasKey) {
    return (
      <p className="gateway-key-set">
        Sending as <code>{key.slice(0, 12)}…</code>
        <button type="button" className="link-button" onClick={() => { setKey(''); setDraft('') }}>
          use another key
        </button>
      </p>
    )
  }

  return (
    <div className="gateway-key">
      <label htmlFor="gateway-key">Team API key</label>
      <p className="gateway-key-why">
        {what} sends real requests to the gateway, which authenticates teams rather
        than people. Your sign-in does not grant it — paste a team key to continue.
      </p>
      <div className="gateway-key-row">
        <input
          id="gateway-key"
          type="password"
          autoComplete="off"
          spellCheck="false"
          placeholder="sk-switchyard-…"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') setKey(draft) }}
        />
        <button type="button" onClick={() => setKey(draft)} disabled={!draft.trim()}>
          Use key
        </button>
      </div>
    </div>
  )
}
