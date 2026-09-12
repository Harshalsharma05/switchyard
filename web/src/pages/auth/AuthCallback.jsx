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

export default function AuthCallback() {
  const { status } = useSession()
  const navigate = useNavigate()

  useEffect(() => {
    if (status === 'loading') return

    let next = '/'
    try {
      next = sessionStorage.getItem(NEXT_PATH_KEY) || '/'
      sessionStorage.removeItem(NEXT_PATH_KEY)
    } catch { /* private mode */ }

    navigate(status === 'signed-in' ? next : '/login?error=signin_failed', { replace: true })
  }, [status, navigate])

  return (
    <div className="auth-callback">
      <div className="auth-spinner" aria-hidden="true" />
      <p role="status">Signing you in…</p>
    </div>
  )
}
