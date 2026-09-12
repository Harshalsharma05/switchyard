// The session context and its hook. Kept separate from the provider component
// so the provider file exports only a component (React Fast Refresh requires it).
import { createContext, useContext } from 'react'

// Where an interrupted navigation is parked while the user signs in, so the
// post-login redirect returns them where they were headed. sessionStorage, not
// a URL parameter: the round trip goes through Google and back.
export const NEXT_PATH_KEY = 'switchyard.nextPath'

export const SessionContext = createContext(null)

export function useSession() {
  const ctx = useContext(SessionContext)
  if (!ctx) throw new Error('useSession must be used within SessionProvider')
  return ctx
}
