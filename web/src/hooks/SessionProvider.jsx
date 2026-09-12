// Session state for the console.
//
// The access token lives in an httpOnly cookie the page cannot read, so there
// is nothing to decode client-side: GET /auth/me on load is what establishes
// who is signed in, and a 401 from it is what proves nobody is.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { refreshSession, setSessionLostHandler } from '../api/client.js'
import { fetchMe, logout as postLogout } from '../api/session.js'
import { SessionContext } from './useSession.js'

// Refresh at three quarters of the token's remaining life, so the reactive
// 401-and-retry path in client.js is a safety net for sleep and suspend rather
// than the normal route.
const REFRESH_AT = 0.75
// Never schedule closer than this; a token already near expiry is refreshed now.
const MIN_DELAY_MS = 5_000

// status: 'loading'    — the /auth/me call is in flight, render nothing yet
//         'signed-out' — show the auth page
//         'signed-in'  — user is set, render the app
export function SessionProvider({ children }) {
  const [status, setStatus] = useState('loading')
  const [user, setUser] = useState(null)
  const [routingOptions, setRoutingOptions] = useState([])
  const timerRef = useRef(null)
  // Lets the scheduled refresh re-arm itself without schedule referencing its
  // own binding before it exists.
  const scheduleRef = useRef(() => {})

  const clearTimer = useCallback(() => {
    if (timerRef.current) {
      clearTimeout(timerRef.current)
      timerRef.current = null
    }
  }, [])

  const applyMe = useCallback((me) => {
    setUser(me.user)
    setRoutingOptions(me.routing_options ?? [])
    setStatus('signed-in')
  }, [])

  const signedOut = useCallback(() => {
    clearTimer()
    setUser(null)
    setRoutingOptions([])
    setStatus('signed-out')
  }, [clearTimer])

  const schedule = useCallback((expiresAt) => {
    clearTimer()
    if (!expiresAt) return
    const remaining = new Date(expiresAt).getTime() - Date.now()
    if (!Number.isFinite(remaining)) return

    timerRef.current = setTimeout(() => {
      refreshSession()
        .then(() => fetchMe())
        .then((me) => {
          applyMe(me)
          scheduleRef.current(me.session_expires_at)
        })
        .catch(() => {
          // A failed proactive refresh is not fatal on its own: the next real
          // request still gets one reactive attempt in client.js, and only that
          // one signs the user out.
        })
    }, Math.max(MIN_DELAY_MS, remaining * REFRESH_AT))
  }, [clearTimer, applyMe])

  useEffect(() => { scheduleRef.current = schedule }, [schedule])

  useEffect(() => {
    const ac = new AbortController()
    fetchMe(ac.signal)
      .then((me) => {
        applyMe(me)
        schedule(me.session_expires_at)
      })
      .catch(() => {
        if (!ac.signal.aborted) signedOut()
      })
    return () => { ac.abort(); clearTimer() }
  }, [applyMe, schedule, signedOut, clearTimer])

  // client.js calls this when a refresh could not rescue a 401.
  useEffect(() => {
    setSessionLostHandler(() => signedOut())
    return () => setSessionLostHandler(null)
  }, [signedOut])

  const signOut = useCallback(async () => {
    try {
      await postLogout()
    } catch {
      // The server clears the cookies either way; a 503 means only that the
      // revoke did not land. Signing out locally regardless is the honest
      // behaviour — the alternative is a button that appears to do nothing.
    }
    signedOut()
  }, [signedOut])

  const value = useMemo(() => ({
    status,
    user,
    routingOptions,
    isSuperadmin: Boolean(user?.is_superadmin),
    signOut,
  }), [status, user, routingOptions, signOut])

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
}
