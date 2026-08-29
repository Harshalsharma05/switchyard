// Routing. Signed-out shows the key screen; signed-in mounts the shell and its
// routes. Admin-only routes are gated here as well as hidden in the rail — a
// non-admin who types the URL is redirected, never shown a broken screen.
import { Navigate, Route, Routes } from 'react-router-dom'
import { useAuth } from './hooks/useAuth.js'
import AppShell from './components/AppShell.jsx'
import SignIn from './pages/SignIn.jsx'
import Overview from './pages/Overview.jsx'
import Playground from './pages/Playground.jsx'
import LiveOps from './pages/LiveOps.jsx'
import RequestLogs from './pages/RequestLogs.jsx'
import UsageCost from './pages/UsageCost.jsx'
import Settings from './pages/Settings.jsx'

function RequireAdmin({ children }) {
  const { isAdmin } = useAuth()
  return isAdmin ? children : <Navigate to="/" replace />
}

export default function App() {
  const { status } = useAuth()

  // 'loading' renders nothing — DESIGN.md forbids a centred spinner, and a
  // flash of the key screen before a restored key validates is worse.
  if (status === 'loading') return null
  if (status !== 'signed-in') return <SignIn />

  return (
    <Routes>
      <Route element={<AppShell />}>
        <Route index element={<Overview />} />
        <Route path="playground" element={<Playground />} />
        <Route path="live-ops" element={<RequireAdmin><LiveOps /></RequireAdmin>} />
        <Route path="logs" element={<RequestLogs />} />
        <Route path="usage" element={<UsageCost />} />
        <Route path="settings" element={<RequireAdmin><Settings /></RequireAdmin>} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}
