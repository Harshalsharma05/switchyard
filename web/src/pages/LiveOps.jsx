// Live Ops — where Part 1's resilience work becomes visible. Admin-only (gated
// in App.jsx). Provider panel (4.1), breaker visualisation (4.2), per-provider
// failure simulation (4.3), and the load simulator (4.4).
import { useCallback } from 'react'
import { fetchProviderHealth } from '../api/health.js'
import { Card } from '../components/primitives.jsx'
import ProviderPanel from '../components/ProviderPanel.jsx'
import BreakerViz from '../components/BreakerViz.jsx'
import LoadSimulator from '../components/LoadSimulator.jsx'
import { ErrorState, Loading } from '../components/states.jsx'
import { useAuth } from '../hooks/useAuth.js'
import { usePolling } from '../hooks/usePolling.js'
import './LiveOps.css'

export default function LiveOps() {
  const { getKey } = useAuth()
  const loadHealth = useCallback((signal) => fetchProviderHealth(getKey(), signal), [getKey])
  const health = usePolling(loadHealth, { interval: 5000 })

  return (
    <>
      <h1 className="page-title">Live Ops</h1>

      <div className="live-ops">
      <Card title="Providers">
        {health.loading && !health.data ? (
          <Loading rows={3} />
        ) : health.error && !health.data ? (
          <ErrorState message="Could not read provider health." onRetry={health.refresh} />
        ) : (
          <ProviderPanel providers={health.data} />
        )}
      </Card>

      <Card title="Circuit breakers">
        {health.loading && !health.data ? (
          <Loading rows={3} />
        ) : health.error && !health.data ? (
          <ErrorState message="Could not read breaker state." onRetry={health.refresh} />
        ) : (
          <BreakerViz providers={health.data} getKey={getKey} onReset={health.refresh} />
        )}
      </Card>

      <Card title="Load simulator">
        <LoadSimulator />
      </Card>
      </div>
    </>
  )
}
