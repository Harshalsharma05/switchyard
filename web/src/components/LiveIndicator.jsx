// Dot + word, always visible in the top bar. It is the user's proof that what
// they are looking at is current.
import { useConnectionStatus } from '../hooks/useConnectionStatus.js'

const LABEL = { live: 'Live', reconnecting: 'Reconnecting', disconnected: 'Disconnected' }

export default function LiveIndicator() {
  const status = useConnectionStatus()
  return (
    <span className={`live live-${status}`} aria-live="polite">
      <span className="live-dot" />
      {LABEL[status]}
    </span>
  )
}
