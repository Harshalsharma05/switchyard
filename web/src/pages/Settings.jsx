// Settings — team management over Part 1's admin API (view teams, adjust limits
// and budgets, key metadata). Admin-only. Built in Phase 6, Step 6.4.
import { EmptyState } from '../components/states.jsx'

export default function Settings() {
  return (
    <>
      <h1 className="page-title">Settings</h1>
      <EmptyState>
        Team management — rate limits, budgets, and API key metadata — will be
        built here in Phase 6.
      </EmptyState>
    </>
  )
}
