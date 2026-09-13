// Settings — open to every signed-in user since Multi-user Step 3.4: managing
// your own organisation's projects and seeing its audit log are both org-admin
// actions, which every signed-in user already is for their own organisation
// (Phase 2, Step 2.4). System info stays superadmin-only — it is deployment
// state, not tenant data — so that section renders only for a superadmin;
// every endpoint below it enforces its own scope regardless of what this
// screen shows.
import { useCallback, useState } from 'react'
import { Card } from '../components/primitives.jsx'
import ProjectTable from '../components/ProjectTable.jsx'
import AuditLog from '../components/AuditLog.jsx'
import SystemPanel from '../components/SystemPanel.jsx'
import { ErrorState, Loading } from '../components/states.jsx'
import { useSession } from '../hooks/useSession.js'
import { useProjectScope } from '../hooks/useProjectScope.js'
import '../components/ProjectTable.css'
import './Settings.css'

export default function Settings() {
  const { isSuperadmin } = useSession()
  // The same project-list poll the top-bar selector uses (Step 3.4) — a
  // project created or deleted here appears in the selector immediately,
  // rather than after its own separate poll interval.
  const { projects, projectsLoading, projectsError, refreshProjects } = useProjectScope()

  // Bumped after any project mutation so the audit log jumps back to page 1
  // and shows the action that was just taken.
  const [auditNonce, setAuditNonce] = useState(0)
  const onChanged = useCallback(() => {
    refreshProjects()
    setAuditNonce((n) => n + 1)
  }, [refreshProjects])

  return (
    <>
      <h1 className="page-title">Settings</h1>

      <div className="settings">
      <Card title="Projects">
        {projectsLoading && !projects ? (
          <Loading rows={3} />
        ) : projectsError && !projects ? (
          <ErrorState message="Could not load projects." onRetry={refreshProjects} />
        ) : (
          <ProjectTable
            projects={projects}
            onChanged={onChanged}
          />
        )}
      </Card>

      <Card title="Audit log">
        {/* Remount on a mutation so the log reopens on page 1 with the new entry. */}
        <AuditLog key={auditNonce} />
      </Card>

      {isSuperadmin && (
        <Card title="System">
          <SystemPanel />
        </Card>
      )}
      </div>
    </>
  )
}
