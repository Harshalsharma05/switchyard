// The three states every data surface needs. Built once here so no screen
// reinvents them and they stay visually consistent (DESIGN.md).
import './states.css'

// Loading — skeleton blocks matching the content's rough shape, never a
// centred spinner. `rows` is how many bars to show.
export function Loading({ rows = 3, className = '' }) {
  return (
    <div className={`state-skeleton ${className}`} aria-busy="true" aria-live="polite">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="state-skeleton-bar" />
      ))}
    </div>
  )
}

// Empty — one line naming what would appear here, plus an optional action.
// No illustration, no apology.
export function EmptyState({ children, action }) {
  return (
    <div className="state-empty">
      <p>{children}</p>
      {action}
    </div>
  )
}

// Error — what failed and what to do, plus a retry. Never a raw error string.
export function ErrorState({ message = 'Something went wrong loading this.', onRetry }) {
  return (
    <div className="state-error" role="alert">
      <p>{message}</p>
      {onRetry && (
        <button type="button" className="state-retry" onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  )
}
