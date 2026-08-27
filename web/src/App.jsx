// Step 2.1: the app is either the key screen or a signed-in stub. The real
// shell — left rail, five routes, top bar — arrives in Step 2.2.
import { useAuth } from './hooks/useAuth.js'
import SignIn from './pages/SignIn.jsx'

export default function App() {
  const { status, me, isAdmin, signOut } = useAuth()

  // 'loading' renders nothing — DESIGN.md forbids a centred spinner, and the
  // check against /admin/me is fast enough that a flash of the key screen is
  // worse than a blank moment.
  if (status === 'loading') return null
  if (status !== 'signed-in') return <SignIn />

  return (
    <main style={{ padding: 'var(--space-6)', display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <h1 style={{ margin: 0, fontSize: 20, fontWeight: 500 }}>
        {me.name}{' '}
        {isAdmin && (
          <span style={{
            fontSize: 12, color: 'var(--status-info)', background: 'var(--status-info-bg)',
            borderRadius: 'var(--radius-control)', padding: '3px 8px', verticalAlign: 'middle',
          }}>
            Admin
          </span>
        )}
      </h1>
      <p style={{ margin: 0, color: 'var(--text-secondary)' }}>
        Signed in as team <span className="num">{me.id}</span>. Shell and screens land in Step 2.2.
      </p>
      <button
        type="button"
        onClick={signOut}
        style={{
          alignSelf: 'flex-start', height: 32, padding: '0 var(--space-3)', fontSize: 13,
          color: 'var(--text-primary)', background: 'transparent',
          border: '1px solid var(--border-strong)', borderRadius: 'var(--radius-control)', cursor: 'pointer',
        }}
      >
        Sign out
      </button>
    </main>
  )
}
