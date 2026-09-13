// The signed-in frame: a fixed icon rail, a top bar, and a centred content
// column that renders the active route. Layout only — screens own their
// content. See DESIGN.md "Shell".
import { useCallback, useMemo, useState } from 'react'
import { NavLink, Outlet, useLocation, useSearchParams } from 'react-router-dom'
import { useSession } from '../hooks/useSession.js'
import { RANGES, TimeRangeContext } from '../hooks/useTimeRange.js'
import { ProjectScopeContext } from '../hooks/useProjectScope.js'
import { fetchProjects } from '../api/projects.js'
import { usePolling } from '../hooks/usePolling.js'
import ChaosProvider from '../hooks/ChaosProvider.jsx'
import ChaosBanner from './ChaosBanner.jsx'
import LiveIndicator from './LiveIndicator.jsx'
import {
  OverviewIcon, PlaygroundIcon, LiveOpsIcon, LogsIcon, UsageIcon, SettingsIcon,
} from './icons.jsx'
import './AppShell.css'

// One entry per route. `admin` items are absent from the rail for non-admins
// and their routes redirect — the gate is enforced in both places (App.jsx).
const NAV = [
  { to: '/', label: 'Overview', Icon: OverviewIcon, end: true },
  { to: '/playground', label: 'Playground', Icon: PlaygroundIcon },
  { to: '/live-ops', label: 'Live Ops', Icon: LiveOpsIcon, admin: true },
  { to: '/logs', label: 'Request Logs', Icon: LogsIcon },
  { to: '/usage', label: 'Usage & Cost', Icon: UsageIcon },
  { divider: true },
  { to: '/settings', label: 'Settings', Icon: SettingsIcon },
]

// Screens that read the shared time range; the selector is hidden elsewhere so
// it never implies it affects a screen it does not. Request Logs and Usage &
// Cost each own their own range control (DESIGN.md), so neither is listed here.
const RANGE_ROUTES = new Set(['/'])

// projectId, when set, is carried onto every rail link's target so the
// selected project survives a screen change (Step 3.3) — otherwise navigating
// away and back would silently reset the scope to "All projects".
function SideRail({ isSuperadmin, projectId }) {
  return (
    <nav className="rail" aria-label="Primary">
      {NAV.map((item, i) =>
        item.divider ? (
          <div key={`d${i}`} className="rail-divider" />
        ) : item.admin && !isSuperadmin ? null : (
          <NavLink
            key={item.to}
            to={projectId ? `${item.to}?project=${encodeURIComponent(projectId)}` : item.to}
            end={item.end}
            className="rail-item"
            title={item.label}
            aria-label={item.label}
          >
            <item.Icon />
          </NavLink>
        ),
      )}
    </nav>
  )
}

function RangeSelector({ range, setRange }) {
  return (
    <div className="rangesel" role="group" aria-label="Time range">
      {RANGES.map((r) => (
        <button
          key={r}
          type="button"
          className={`rangesel-btn num ${r === range ? 'active' : ''}`}
          aria-pressed={r === range}
          onClick={() => setRange(r)}
        >
          {r}
        </button>
      ))}
    </div>
  )
}

// The project scope selector (Step 3.3): "All projects" first and the
// default, then one option per project the caller can see — their own
// organisation's, or every organisation's for a superadmin, the same list
// Settings and Usage & Cost already draw from.
function ProjectSelector({ projects, projectId, onChange }) {
  return (
    <select
      className="projectsel num"
      aria-label="Project"
      value={projectId ?? ''}
      onChange={(e) => onChange(e.target.value || null)}
    >
      <option value="">All projects</option>
      {projects.map((p) => (
        <option key={p.id} value={p.id}>{p.name}</option>
      ))}
    </select>
  )
}

function TopBar({
  user, isSuperadmin, onSignOut, range, setRange, showRange, projects, projectId, setProjectId,
}) {
  return (
    <header className="topbar">
      <span className="topbar-brand">SwitchYard</span>
      <span className="topbar-team">
        {user?.name || user?.email}
        {isSuperadmin && <span className="pill pill-info">Superadmin</span>}
      </span>
      <div className="topbar-right">
        <ProjectSelector projects={projects} projectId={projectId} onChange={setProjectId} />
        {showRange && <RangeSelector range={range} setRange={setRange} />}
        <LiveIndicator />
        <button type="button" className="topbar-signout" onClick={onSignOut}>
          Sign out
        </button>
      </div>
    </header>
  )
}

export default function AppShell() {
  const { user, isSuperadmin, signOut } = useSession()
  const { pathname } = useLocation()
  const [range, setRange] = useState('24h')
  const timeRange = useMemo(() => ({ range, setRange }), [range])

  // The project scope lives in the URL, not component state, so a scoped
  // view is shareable and survives a refresh (Step 3.3). AppShell owns it —
  // as the parent of every routed screen, it is the one place a value can be
  // read off the current location and carried onto the next one.
  const [searchParams, setSearchParams] = useSearchParams()
  const projectId = searchParams.get('project') || null

  const setProjectId = useCallback((id) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      if (id) next.set('project', id)
      else next.delete('project')
      return next
    }, { replace: true })
  }, [setSearchParams])

  // Shared with Settings (Step 3.4) via context, not fetched twice: creating
  // or deleting a project there calls refreshProjects so the top bar's
  // selector picks it up on the same round trip instead of waiting out this
  // poll's own interval. `projects` stays the raw, nullable poll result
  // (rather than defaulted to `[]`) so a consumer that needs to tell "nothing
  // loaded yet" apart from "this organisation genuinely has none" still can —
  // Settings needs exactly that distinction to choose between its loading,
  // error, and empty states, the same way it already does for its own data.
  const loadProjects = useCallback((signal) => fetchProjects(signal), [])
  const projectsPoll = usePolling(loadProjects, { interval: 30000 })

  const projectScope = useMemo(
    () => ({
      projectId,
      setProjectId,
      projects: projectsPoll.data,
      projectsLoading: projectsPoll.loading,
      projectsError: projectsPoll.error,
      refreshProjects: projectsPoll.refresh,
    }),
    [projectId, setProjectId, projectsPoll.data, projectsPoll.loading, projectsPoll.error, projectsPoll.refresh],
  )

  // Chaos is an admin-only, dev-only concern; a non-admin shell has no provider
  // and no banner.
  const content = (
    <main className="content">
      <ChaosBanner />
      <Outlet />
    </main>
  )

  return (
    <ProjectScopeContext.Provider value={projectScope}>
      <TimeRangeContext.Provider value={timeRange}>
        <div className="shell">
          <SideRail isSuperadmin={isSuperadmin} projectId={projectId} />
          <div className="shell-main">
            <TopBar
              user={user}
              isSuperadmin={isSuperadmin}
              onSignOut={signOut}
              range={range}
              setRange={setRange}
              showRange={RANGE_ROUTES.has(pathname)}
              projects={projectScope.projects ?? []}
              projectId={projectId}
              setProjectId={setProjectId}
            />
            {isSuperadmin ? <ChaosProvider>{content}</ChaosProvider> : content}
          </div>
        </div>
      </TimeRangeContext.Provider>
    </ProjectScopeContext.Provider>
  )
}
