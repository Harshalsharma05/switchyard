// Shared chaos-harness state: whether fault injection is available on this
// gateway, and the rules currently in force. Polled once in the shell so the
// warning banner (every screen) and Live Ops' controls read the same data.
import { createContext, useContext } from 'react'

export const ChaosContext = createContext(null)

// Returns null outside the provider (e.g. a non-admin shell), so callers guard.
export function useChaos() {
  return useContext(ChaosContext)
}
