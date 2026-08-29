// Live Ops — provider panel, circuit-breaker visualisation, failure simulation,
// and the load simulator. Admin-only. Built in Phase 4.
import { EmptyState } from '../components/states.jsx'

export default function LiveOps() {
  return (
    <>
      <h1 className="page-title">Live Ops</h1>
      <EmptyState>
        Provider health, breaker state, chaos controls, and the load simulator
        will be built here in Phase 4.
      </EmptyState>
    </>
  )
}
