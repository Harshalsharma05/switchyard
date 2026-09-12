// Routing. Signed-out sends every app route to the auth page, remembering
// where the user was headed; signed-in mounts the shell. Superadmin-only routes
// are gated here as well as hidden in the rail — someone who types the URL is
// redirected, never shown a broken screen.
import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { NEXT_PATH_KEY, useSession } from './hooks/useSession.js'
import AppShell from './components/AppShell.jsx'
import AuthPage from './pages/auth/AuthPage.jsx'
import AuthCallback from './pages/auth/AuthCallback.jsx'
import Overview from './pages/Overview.jsx'
import Playground from './pages/Playground.jsx'
import LiveOps from './pages/LiveOps.jsx'
import RequestLogs from './pages/RequestLogs.jsx'
import UsageCost from './pages/UsageCost.jsx'
import Settings from './pages/Settings.jsx'

function RequireSuperadmin({ children }) {
  const { isSuperadmin } = useSession()
  return isSuperadmin ? children : <Navigate to="/" replace />
}

// Parks the interrupted path so the post-login redirect can return there. It
// goes in sessionStorage rather than the URL because the round trip leaves this
// origin entirely and comes back through Google.
function RequireSession({ children }) {
  const { status } = useSession()
  const location = useLocation()

  if (status === 'signed-in') return children
  const path = location.pathname + location.search
  if (path !== '/') {
    try { sessionStorage.setItem(NEXT_PATH_KEY, path) } catch { /* private mode */ }
  }
  return <Navigate to="/login" replace />
}

export default function App() {
  const { status } = useSession()

  // 'loading' renders nothing anywhere else — DESIGN.md forbids a centred
  // spinner, and a flash of the login page before /auth/me answers is worse
  // than a blank beat.
  //
  // /signing-in is the exception, and has to be: it is the interstitial for
  // exactly this window. Bailing out here would leave the browser blank for the
  // whole round trip after Google hands the user back, which reads as a crash.
  if (status === 'loading') {
    return (
      <Routes>
        <Route path="/signing-in" element={<AuthCallback />} />
        <Route path="*" element={null} />
      </Routes>
    )
  }

  return (
    <Routes>
      <Route path="/login" element={status === 'signed-in' ? <Navigate to="/" replace /> : <AuthPage />} />
      <Route path="/signing-in" element={<AuthCallback />} />

      <Route element={<RequireSession><AppShell /></RequireSession>}>
        <Route index element={<Overview />} />
        <Route path="playground" element={<Playground />} />
        <Route path="live-ops" element={<RequireSuperadmin><LiveOps /></RequireSuperadmin>} />
        <Route path="logs" element={<RequestLogs />} />
        <Route path="usage" element={<UsageCost />} />
        <Route path="settings" element={<RequireSuperadmin><Settings /></RequireSuperadmin>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}
