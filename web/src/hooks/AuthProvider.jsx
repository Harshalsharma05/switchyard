// Session state for the console. Part 1's teams are the identity model — there
// is no login backend. The user pastes a team key; it lives in memory and in
// sessionStorage (per-tab, gone when the tab closes), and every call sends it
// as a bearer token. A successful GET /admin/me is what proves a key is valid.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { ApiError } from '../api/client.js'
import { fetchMe } from '../api/session.js'
import { AuthContext, STORAGE_KEY } from './useAuth.js'

function readStoredKey() {
  try { return sessionStorage.getItem(STORAGE_KEY) } catch { return null }
}

// status: 'loading'    — validating a restored key, render nothing yet
//         'signed-out' — show the key screen (error holds why, if any)
//         'signed-in'  — key + me are set, render the app
export function AuthProvider({ children }) {
  const [restoredKey] = useState(readStoredKey)
  const [status, setStatus] = useState(restoredKey ? 'loading' : 'signed-out')
  const [me, setMe] = useState(null)
  const [error, setError] = useState(null)
  const keyRef = useRef(null)

  const apply = useCallback((key, identity) => {
    keyRef.current = key
    setMe(identity)
    setError(null)
    setStatus('signed-in')
  }, [])

  const clear = useCallback((why) => {
    keyRef.current = null
    setMe(null)
    setError(why ?? null)
    setStatus('signed-out')
    try { sessionStorage.removeItem(STORAGE_KEY) } catch { /* private mode */ }
  }, [])

  // Validate a key restored from this tab's session, once.
  useEffect(() => {
    if (!restoredKey) return
    const ac = new AbortController()
    fetchMe(restoredKey, ac.signal)
      .then((identity) => apply(restoredKey, identity))
      .catch((err) => {
        if (ac.signal.aborted) return
        clear(err instanceof ApiError && err.status === 401 ? 'your saved key is no longer valid' : null)
      })
    return () => ac.abort()
  }, [restoredKey, apply, clear])

  const signIn = useCallback(async (key) => {
    const trimmed = key.trim()
    if (!trimmed) { setError('enter a team key'); return }
    setStatus('loading')
    try {
      const identity = await fetchMe(trimmed)
      try { sessionStorage.setItem(STORAGE_KEY, trimmed) } catch { /* private mode */ }
      apply(trimmed, identity)
    } catch (err) {
      const msg = err instanceof ApiError && err.status === 401
        ? 'that key was not recognized'
        : err instanceof ApiError && err.type === 'unreachable'
          ? 'could not reach the gateway — is it running?'
          : 'sign in failed, try again'
      clear(msg)
    }
  }, [apply, clear])

  // A function, not a value, so the key stays out of React state and render.
  const getKey = useCallback(() => keyRef.current, [])
  const signOut = useCallback(() => clear(null), [clear])

  // Memoized: the polling hooks key their effects on the fetchers built from
  // getKey, so a context value rebuilt on every render would restart every
  // poll on every render.
  const value = useMemo(
    () => ({ status, me, error, isAdmin: me?.is_admin ?? false, signIn, signOut, getKey }),
    [status, me, error, signIn, signOut, getKey],
  )
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}
