// The shared project scope the top bar's selector sets and every
// project-aware screen (Overview, Request Logs, Usage & Cost) reads.
// Selection lives in the URL (?project=<id>), not just this context, so a
// scoped view is shareable and survives a refresh — the context exists only
// so screens don't each re-parse the query string themselves.
//
// The context also carries AppShell's own project *list* poll — projects,
// projectsLoading, projectsError, refreshProjects — so Settings (Step 3.4)
// reads and refreshes the same fetch the top-bar selector uses instead of
// polling /admin/teams a second time, and a project created in Settings
// appears in the selector on the same round trip rather than waiting out a
// separate poll interval.
import { createContext, useContext } from 'react'

export const ProjectScopeContext = createContext(null)

export function useProjectScope() {
  const ctx = useContext(ProjectScopeContext)
  if (!ctx) throw new Error('useProjectScope must be used within AppShell')
  return ctx
}
