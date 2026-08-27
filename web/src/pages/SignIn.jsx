// The key screen. No accounts, no password — a team pastes the API key it was
// given out of band, and a valid GET /admin/me lets it in.
import { useState } from 'react'
import { useAuth } from '../hooks/useAuth.js'
import './SignIn.css'

export default function SignIn() {
  const { signIn, error, status } = useAuth()
  const [key, setKey] = useState('')
  const busy = status === 'loading'

  return (
    <main className="signin">
      <form
        className="signin-card"
        onSubmit={(e) => { e.preventDefault(); signIn(key) }}
      >
        <h1>SwitchYard console</h1>
        <p className="signin-sub">Sign in with your team API key.</p>

        <input
          className="signin-input num"
          type="password"
          autoComplete="off"
          autoFocus
          placeholder="sk-switchyard-…"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          aria-invalid={error ? 'true' : undefined}
        />

        {error && <p className="signin-error" role="alert">{error}</p>}

        <button className="signin-submit" type="submit" disabled={busy}>
          {busy ? 'Checking…' : 'Continue'}
        </button>
      </form>
    </main>
  )
}
