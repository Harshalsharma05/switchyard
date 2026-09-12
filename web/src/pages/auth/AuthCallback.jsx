// Where the gateway lands the browser after a successful Google exchange.
//
// Routed at /signing-in, deliberately outside /auth: the console proxies that
// whole prefix to the gateway, so a client-side route under it would never
// reach React.
//
// The cookies are already set by the time this renders; all that is left is to
// let the session load and send the user where they were originally headed. It
// exists mainly so the gap is not a blank screen, which reads as a crash.
import { useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { NEXT_PATH_KEY, useSession } from '../../hooks/useSession.js'
import './auth.css'

// Read once per page load and remembered here, because the read consumes the
// value. StrictMode double-invokes effects in development, and a second read
// would find nothing and send the user to "/" instead of where they were going.
// Module state is the right scope: a real sign-in is a full document load, so
// this resets naturally.
let captured = null

function takeNextPath() {
  if (captured === null) {
    try {
      captured = sessionStorage.getItem(NEXT_PATH_KEY) || '/'
      sessionStorage.removeItem(NEXT_PATH_KEY)
    } catch {
      captured = '/'
    }
  }
  return captured
}

export default function AuthCallback() {
  const { status } = useSession()
  const navigate = useNavigate()

  useEffect(() => {
    if (status === 'loading') return
    const next = takeNextPath()
    navigate(status === 'signed-in' ? next : '/login?error=signin_failed', { replace: true })
  }, [status, navigate])

  return (
    <div className="auth-callback">
      <div className="auth-spinner" aria-hidden="true" />
      <p role="status">Signing you in…</p>
    </div>
  )
}
