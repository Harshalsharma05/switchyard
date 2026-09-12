// Sign-in. Split screen: a decorative brand panel on the left, the form on the
// right. Below 900px the brand panel is removed entirely rather than stacked —
// a tagline above a login form on a narrow screen only pushes the form down.
import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { GOOGLE_SIGNIN_PATH } from '../../api/session.js'
import BrandPanel from './BrandPanel.jsx'
import GoogleMark from './GoogleMark.jsx'
import './auth.css'

// Every sign-in failure the browser is told about collapses into one message.
// A distinguishable reason would say which addresses are registered.
const GENERIC_ERROR = 'We could not sign you in. Please try again.'

export default function AuthPage() {
  const [params] = useSearchParams()
  const [redirecting, setRedirecting] = useState(false)
  const error = params.get('error') ? GENERIC_ERROR : null

  // A full navigation, not a fetch: the flow leaves this origin for Google and
  // returns to the gateway's callback, which sets the cookies.
  const signIn = () => {
    setRedirecting(true)
    window.location.assign(GOOGLE_SIGNIN_PATH)
  }

  return (
    <div className="auth">
      <BrandPanel />

      <main className="auth-panel">
        <div className="auth-form">
          <h1>Sign in</h1>
          <p className="auth-sub">Continue to your SwitchYard console.</p>

          {error && <p className="auth-error" role="alert">{error}</p>}

          <button
            type="button"
            className="auth-google"
            onClick={signIn}
            disabled={redirecting}
            autoFocus
          >
            {redirecting
              ? <><span className="auth-btn-spinner" aria-hidden="true" />Redirecting…</>
              : <><GoogleMark />Continue with Google</>}
          </button>

          <p className="auth-note">
            First time here? Signing in with Google creates your account and its
            organisation — there is nothing else to fill in.
          </p>

          <p className="auth-footer">
            Bring your own provider keys. We never resell tokens.
          </p>
        </div>
      </main>
    </div>
  )
}
