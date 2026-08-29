// The signed-in frame: a fixed icon rail, a top bar, and a centred content
// column that renders the active route. Layout only — screens own their
// content. See DESIGN.md "Shell".
import { useMemo, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth.js'
import { RANGES, TimeRangeContext } from '../hooks/useTimeRange.js'
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
  { to: '/settings', label: 'Settings', Icon: SettingsIcon, admin: true },
]

// Screens that read the shared time range; the selector is hidden elsewhere so
// it never implies it affects a screen it does not.
const RANGE_ROUTES = new Set(['/', '/logs', '/usage'])

function SideRail({ isAdmin }) {
  return (
    <nav className="rail" aria-label="Primary">
      {NAV.map((item, i) =>
        item.divider ? (
          <div key={`d${i}`} className="rail-divider" />
        ) : item.admin && !isAdmin ? null : (
          <NavLink
            key={item.to}
            to={item.to}
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

function TopBar({ team, isAdmin, onSignOut, range, setRange, showRange }) {
  return (
    <header className="topbar">
      <span className="topbar-brand">SwitchYard</span>
      <span className="topbar-team">
        {team?.name}
        {isAdmin && <span className="pill pill-info">Admin</span>}
      </span>
      <div className="topbar-right">
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
  const { me, isAdmin, signOut } = useAuth()
  const { pathname } = useLocation()
  const [range, setRange] = useState('24h')
  const timeRange = useMemo(() => ({ range, setRange }), [range])

  return (
    <TimeRangeContext.Provider value={timeRange}>
      <div className="shell">
        <SideRail isAdmin={isAdmin} />
        <div className="shell-main">
          <TopBar
            team={me}
            isAdmin={isAdmin}
            onSignOut={signOut}
            range={range}
            setRange={setRange}
            showRange={RANGE_ROUTES.has(pathname)}
          />
          <main className="content">
            <Outlet />
          </main>
        </div>
      </div>
    </TimeRangeContext.Provider>
  )
}
