// Polls /admin/chaos for the shell and exposes apply/clear helpers. Only
// mounted for admins; a non-admin shell renders children with no provider and
// useChaos() returns null.
import { useCallback, useMemo } from 'react'
import { ChaosContext } from './useChaos.js'
import { clearChaos, fetchChaos, setChaosRules } from '../api/chaos.js'
import { usePolling } from './usePolling.js'

// A 404 is the gateway saying chaos was never compiled in for this environment
// — permanent, so usePolling stops rather than retrying and misreporting it as
// a connection problem.
const isUnavailable = (err) => err.status === 404

export default function ChaosProvider({ getKey, children }) {
  const load = useCallback((signal) => fetchChaos(getKey(), signal), [getKey])
  const poll = usePolling(load, { interval: 5000, ignoreError: isUnavailable })

  const unavailable = poll.error?.status === 404
  const available = unavailable ? false : (poll.data?.available ?? false)
  const rules = useMemo(() => poll.data?.rules ?? [], [poll.data])

  const applyRules = useCallback(
    async (next) => {
      await setChaosRules(getKey(), next)
      poll.refresh()
    },
    [getKey, poll],
  )

  const clearAll = useCallback(async () => {
    await clearChaos(getKey())
    poll.refresh()
  }, [getKey, poll])

  const value = useMemo(
    () => ({ available, rules, applyRules, clearAll, refresh: poll.refresh }),
    [available, rules, applyRules, clearAll, poll.refresh],
  )

  return <ChaosContext.Provider value={value}>{children}</ChaosContext.Provider>
}
