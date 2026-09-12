// Settings (Step 6.4) — admin-only, gated in App.jsx and hidden from the rail for
// non-admins; every endpoint this screen calls is 403 for a non-admin key, which
// is the real enforcement. Four sections per DESIGN.md: teams (with inline edit
// and per-team key actions), the audit log, and system info.
import { useCallback, useState } from 'react'
import { fetchTeams } from '../api/teams.js'
import { Card } from '../components/primitives.jsx'
import TeamTable from '../components/TeamTable.jsx'
import AuditLog from '../components/AuditLog.jsx'
import SystemPanel from '../components/SystemPanel.jsx'
import { ErrorState, Loading } from '../components/states.jsx'
import { usePolling } from '../hooks/usePolling.js'
import '../components/TeamTable.css'
import './Settings.css'

export default function Settings() {

  const loadTeams = useCallback((signal) => fetchTeams(signal), [])
  const teams = usePolling(loadTeams, { interval: 30000 })

  // Bumped after any team mutation so the audit log jumps back to page 1 and
  // shows the action that was just taken.
  const [auditNonce, setAuditNonce] = useState(0)
  const onChanged = useCallback(() => {
    teams.refresh()
    setAuditNonce((n) => n + 1)
  }, [teams])

  return (
    <>
      <h1 className="page-title">Settings</h1>

      <div className="settings">
      <Card title="Teams">
        {teams.loading && !teams.data ? (
          <Loading rows={3} />
        ) : teams.error && !teams.data ? (
          <ErrorState message="Could not load teams." onRetry={teams.refresh} />
        ) : (
          <TeamTable
            teams={teams.data}
            onChanged={onChanged}
          />
        )}
      </Card>

      <Card title="Audit log">
        {/* Remount on a mutation so the log reopens on page 1 with the new entry. */}
        <AuditLog key={auditNonce} />
      </Card>

      <Card title="System">
        <SystemPanel />
      </Card>
      </div>
    </>
  )
}
