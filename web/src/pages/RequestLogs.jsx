// Request Logs — the filterable table over Phase 1's query API, with a row
// detail drawer and a Jaeger deep link. Built in Phase 5.
import { EmptyState } from '../components/states.jsx'

export default function RequestLogs() {
  return (
    <>
      <h1 className="page-title">Request logs</h1>
      <EmptyState>
        A filterable, paginated table of every logged request will be built
        here in Phase 5.
      </EmptyState>
    </>
  )
}
