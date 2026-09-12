// A team API key for the two screens that call the gateway port.
//
// Playground and the Live Ops load simulator send real completions to :8080,
// which authenticates teams, not people. A dashboard session authorises nothing
// there — that separation is the point of the two ports — so these screens need
// a team key of their own, supplied by the operator.
//
// Per-tab and never sent to the admin port. Kept out of SessionProvider on
// purpose: mixing the two credentials in one place is how they start leaking
// into each other.
import { useCallback, useState } from 'react'

export const GATEWAY_KEY_STORAGE = 'switchyard.gatewayKey'

function read() {
  try { return sessionStorage.getItem(GATEWAY_KEY_STORAGE) ?? '' } catch { return '' }
}

export function useGatewayKey() {
  const [key, setKey] = useState(read)

  const save = useCallback((next) => {
    const trimmed = next.trim()
    setKey(trimmed)
    try {
      if (trimmed) sessionStorage.setItem(GATEWAY_KEY_STORAGE, trimmed)
      else sessionStorage.removeItem(GATEWAY_KEY_STORAGE)
    } catch { /* private mode */ }
  }, [])

  return { key, setKey: save, hasKey: Boolean(key) }
}
