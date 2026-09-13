// Project management (Settings §1–2). A table over the admin team API: inline
// edit for rate limits and budget, per-project budget reset, per-project key
// rotate / revoke, project delete, and a create form (Tier 1, Step 2.6). No
// optimistic UI — every action shows a pending state and then the server's
// actual response (DESIGN.md). Every confirmation is inline, never a modal.
// The Key column shows a masked display form for a key this gateway minted
// and the source otherwise — never the key, never the hash.
import { useEffect, useState } from 'react'
import {
  createProject, deleteProject, patchProject, resetProjectBudget, revokeProjectKey, rotateProjectKey,
} from '../api/projects.js'
import { fetchProviders } from '../api/providers.js'
import { useSession } from '../hooks/useSession.js'
import { formatUSD } from '../utils/format.js'
import './ProjectTable.css'

const COLS = 10

const PRIORITIES = [
  ['realtime', 'Realtime'],
  ['batch', 'Batch — shed first near a limit'],
]

function nonNegative(s) {
  const n = Number(s)
  return s.trim() !== '' && Number.isFinite(n) && n >= 0 ? n : null
}

function positive(s, { integer = false } = {}) {
  const n = Number(s)
  if (s.trim() === '' || !Number.isFinite(n) || n <= 0) return null
  if (integer && !Number.isInteger(n)) return null
  return n
}

function keyLabel(key) {
  if (key?.masked) return key.masked
  if (key?.source === 'revoked') return 'revoked'
  return 'config'
}

// The show-once panel: the plaintext key, a copy button, the server's warning,
// and an explicit "not shown again" line. Dismiss collapses it back to masked.
// Shared by key rotation and project creation — one panel, one contract.
function ShowOncePanel({ result, onDismiss }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(result.api_key)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      setCopied(false)
    }
  }
  return (
    <div className="pt-showonce">
      <div className="pt-showonce-head">New API key — copy it now</div>
      <div className="pt-showonce-keyrow">
        <code className="pt-showonce-key num">{result.api_key}</code>
        <button type="button" className="pt-btn pt-btn-primary" onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <p className="pt-showonce-warn">
        This key will not be shown again. {result.warning}
      </p>
      <button type="button" className="pt-btn" onClick={onDismiss}>Dismiss</button>
    </div>
  )
}

function ProjectRow({ project, callerProjectId, onChanged }) {
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState({ rpm: '', tpm: '', budget: '' })
  const [busy, setBusy] = useState(null) // 'save' | 'reset' | 'key' | 'delete'
  const [error, setError] = useState(null)
  const [resetPhase, setResetPhase] = useState(null) // null | 'confirm'
  const [keyPhase, setKeyPhase] = useState(null) // null | 'rotate' | 'revoke'
  const [deletePhase, setDeletePhase] = useState(null) // null | 'confirm' | 'self'
  const [rotated, setRotated] = useState(null) // { api_key, key, warning }

  const rl = project.rate_limits
  const isSelf = project.id === callerProjectId

  const startEdit = () => {
    setForm({ rpm: String(rl.rpm), tpm: String(rl.tpm), budget: String(project.monthly_budget_usd) })
    setError(null)
    setEditing(true)
  }
  const cancel = () => { setEditing(false); setError(null) }

  const save = async () => {
    const rpm = nonNegative(form.rpm)
    const tpm = nonNegative(form.tpm)
    const budget = nonNegative(form.budget)
    if (rpm === null || tpm === null || budget === null) {
      setError('RPM, TPM and budget must be non-negative numbers')
      return
    }
    const patch = {}
    if (rpm !== rl.rpm) patch.rpm = rpm
    if (tpm !== rl.tpm) patch.tpm = tpm
    if (budget !== project.monthly_budget_usd) patch.monthly_budget_usd = budget
    if (Object.keys(patch).length === 0) { setEditing(false); return }

    setBusy('save')
    setError(null)
    try {
      await patchProject(project.id, patch)
      setEditing(false)
      onChanged()
    } catch (e) {
      setError(e.message || 'the change was not applied')
    } finally {
      setBusy(null)
    }
  }

  const reset = async () => {
    setResetPhase(null)
    setBusy('reset')
    setError(null)
    try {
      await resetProjectBudget(project.id)
      onChanged()
    } catch (e) {
      setError(e.message || 'the reset was not applied')
    } finally {
      setBusy(null)
    }
  }

  const rotate = async () => {
    setKeyPhase(null)
    setBusy('key')
    setError(null)
    try {
      const result = await rotateProjectKey(project.id)
      setRotated(result)
      onChanged()
    } catch (e) {
      setError(e.message || 'the key was not rotated')
    } finally {
      setBusy(null)
    }
  }

  const revoke = async () => {
    setKeyPhase(null)
    setBusy('key')
    setError(null)
    try {
      await revokeProjectKey(project.id)
      onChanged()
    } catch (e) {
      setError(e.message || 'the key was not revoked')
    } finally {
      setBusy(null)
    }
  }

  // Delete stays enabled on your own row and explains itself on click, rather
  // than being disabled silently (DESIGN.md). The server refuses it either way.
  const askDelete = () => {
    setError(null)
    setDeletePhase(isSelf ? 'self' : 'confirm')
  }

  const remove = async () => {
    setDeletePhase(null)
    setBusy('delete')
    setError(null)
    try {
      await deleteProject(project.id)
      onChanged() // the refreshed list no longer contains this row
    } catch (e) {
      setError(e.message || 'the project was not deleted')
      setBusy(null)
    }
  }

  const field = (name) => (
    <input
      className="pt-input num"
      type="number"
      min="0"
      value={form[name]}
      disabled={busy === 'save'}
      onChange={(e) => setForm((f) => ({ ...f, [name]: e.target.value }))}
    />
  )

  const models = project.allowed_models ?? []

  let actions
  if (editing) {
    actions = (
      <>
        <button type="button" className="pt-btn pt-btn-primary" onClick={save} disabled={busy === 'save'}>
          {busy === 'save' ? 'Saving…' : 'Save'}
        </button>
        <button type="button" className="pt-btn" onClick={cancel} disabled={busy === 'save'}>
          Cancel
        </button>
      </>
    )
  } else if (resetPhase === 'confirm') {
    actions = (
      <span className="pt-confirm">
        Reset {project.name}'s spend to $0.00?
        <button type="button" className="pt-btn pt-btn-sm pt-btn-danger" onClick={reset}>Reset</button>
        <button type="button" className="pt-btn pt-btn-sm" onClick={() => setResetPhase(null)}>Cancel</button>
      </span>
    )
  } else if (deletePhase === 'confirm') {
    actions = (
      <span className="pt-confirm">
        Delete {project.name}? Its key stops working immediately, and the name cannot be reused.
        <button type="button" className="pt-btn pt-btn-sm pt-btn-danger" onClick={remove}>Delete</button>
        <button type="button" className="pt-btn pt-btn-sm" onClick={() => setDeletePhase(null)}>Cancel</button>
      </span>
    )
  } else if (deletePhase === 'self') {
    actions = (
      <span className="pt-confirm">
        You cannot delete the project you are signed in as. Sign in with another admin key to delete it.
        <button type="button" className="pt-btn pt-btn-sm" onClick={() => setDeletePhase(null)}>OK</button>
      </span>
    )
  } else if (busy === 'delete') {
    actions = <span className="pt-muted">Deleting…</span>
  } else {
    actions = (
      <>
        <button type="button" className="pt-btn" onClick={startEdit} disabled={busy != null}>
          Edit
        </button>
        <button type="button" className="pt-btn" onClick={() => setResetPhase('confirm')} disabled={busy != null}>
          {busy === 'reset' ? 'Resetting…' : 'Reset budget'}
        </button>
        <button type="button" className="pt-btn" onClick={askDelete} disabled={busy != null}>
          Delete
        </button>
      </>
    )
  }

  return (
    <>
      <tr className={editing ? 'pt-row-editing' : ''}>
        <td>
          <span className="pt-name">{project.name}</span>
          <span className="pt-id num">{project.id}</span>
        </td>
        <td className="pt-key">
          <span className="num pt-key-label" title={`key source: ${project.key?.source ?? 'config'}`}>
            {keyLabel(project.key)}
          </span>
          {keyPhase === null && busy !== 'key' && (
            <span className="pt-key-actions">
              <button type="button" className="pt-btn pt-btn-sm" onClick={() => setKeyPhase('rotate')} disabled={busy != null}>
                Rotate
              </button>
              <button
                type="button"
                className="pt-btn pt-btn-sm"
                onClick={() => setKeyPhase('revoke')}
                disabled={busy != null || isSelf}
                title={isSelf ? 'You cannot revoke the key you are signed in with' : undefined}
              >
                Revoke
              </button>
            </span>
          )}
          {busy === 'key' && <span className="pt-muted">Working…</span>}
          {keyPhase === 'rotate' && (
            <span className="pt-confirm">
              Rotate this key? The current key stops working immediately.
              <button type="button" className="pt-btn pt-btn-sm pt-btn-primary" onClick={rotate}>Rotate</button>
              <button type="button" className="pt-btn pt-btn-sm" onClick={() => setKeyPhase(null)}>Cancel</button>
            </span>
          )}
          {keyPhase === 'revoke' && (
            <span className="pt-confirm">
              Revoke this key? {project.name} will have no working key until you rotate one.
              <button type="button" className="pt-btn pt-btn-sm pt-btn-danger" onClick={revoke}>Revoke</button>
              <button type="button" className="pt-btn pt-btn-sm" onClick={() => setKeyPhase(null)}>Cancel</button>
            </span>
          )}
        </td>
        <td>{project.priority}</td>
        <td className="num ta-r">{editing ? field('rpm') : rl.rpm.toLocaleString()}</td>
        <td className="num ta-r">{editing ? field('tpm') : rl.tpm.toLocaleString()}</td>
        <td className="num ta-r">{editing ? field('budget') : formatUSD(project.monthly_budget_usd)}</td>
        <td className="num ta-r">{project.spent_usd == null ? '—' : formatUSD(project.spent_usd)}</td>
        <td className="num" title={models.join(', ')}>
          {models.length} model{models.length === 1 ? '' : 's'}
        </td>
        <td>
          {project.is_admin
            ? <span className="pt-admin">Admin</span>
            : <span className="pt-muted">·</span>}
        </td>
        <td className="pt-actions">{actions}</td>
      </tr>
      {rotated && (
        <tr className="pt-showonce-row">
          <td colSpan={COLS}>
            <ShowOncePanel result={rotated} onDismiss={() => setRotated(null)} />
          </td>
        </tr>
      )}
      {error && (
        <tr className="pt-error-row">
          <td colSpan={COLS} role="alert">{error}</td>
        </tr>
      )}
    </>
  )
}

// The create form. Models are offered grouped under the providers that are
// ticked, so a model can only be allowed alongside a provider that serves it.
//
// The organisation field is superadmin-only (Multi-user, Step 3.4): the
// server always creates a non-superadmin's project in their own organisation
// regardless of what this form sends, so showing a picker with nothing real
// to pick would be misleading rather than just unnecessary — an org with no
// projects yet would render an empty select. A superadmin genuinely chooses
// among the real organisations its own projects belong to.
function CreateProjectForm({ projects, onCreated, onCancel }) {
  const { isSuperadmin } = useSession()
  const orgs = isSuperadmin
    ? [...new Set(projects.map((p) => p.organization_id).filter(Boolean))].sort()
    : []

  const [form, setForm] = useState({
    name: '', org: orgs[0] ?? '', priority: 'realtime', rpm: '', tpm: '', budget: '', isAdmin: false,
  })
  const [catalogue, setCatalogue] = useState(null) // [{ name, models }] once loaded
  const [catalogueError, setCatalogueError] = useState(null)
  const [providers, setProviders] = useState([])
  const [models, setModels] = useState([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)

  useEffect(() => {
    const ac = new AbortController()
    fetchProviders(ac.signal)
      .then((list) => setCatalogue(list.map((p) => ({ name: p.name, models: p.models ?? [] }))))
      .catch((e) => {
        if (e.name !== 'AbortError') setCatalogueError('Could not load the provider list.')
      })
    return () => ac.abort()
  }, [])

  const set = (name) => (e) => {
    const value = e.target.type === 'checkbox' ? e.target.checked : e.target.value
    setForm((f) => ({ ...f, [name]: value }))
  }

  const modelsFor = (names) => new Set(
    (catalogue ?? []).filter((p) => names.includes(p.name)).flatMap((p) => p.models),
  )

  const toggleProvider = (name) => {
    const next = providers.includes(name) ? providers.filter((p) => p !== name) : [...providers, name]
    setProviders(next)
    // Unticking a provider drops the models only it served.
    const stillOffered = modelsFor(next)
    setModels((ms) => ms.filter((m) => stillOffered.has(m)))
  }

  const toggleModel = (name) => {
    setModels((ms) => (ms.includes(name) ? ms.filter((m) => m !== name) : [...ms, name]))
  }

  const submit = async (e) => {
    e.preventDefault()
    const rpm = positive(form.rpm, { integer: true })
    const tpm = positive(form.tpm, { integer: true })
    const budget = positive(form.budget)
    let problem = null
    if (!form.name.trim()) problem = 'Name is required.'
    else if (rpm === null || tpm === null) problem = 'RPM and TPM must be whole numbers above zero.'
    else if (budget === null) problem = 'Monthly budget must be above zero.'
    else if (providers.length === 0) problem = 'Allow at least one provider.'
    else if (models.length === 0) problem = 'Allow at least one model.'
    if (problem) { setError(problem); return }

    const body = {
      name: form.name.trim(),
      priority: form.priority,
      rpm,
      tpm,
      monthly_budget_usd: budget,
      allowed_providers: providers,
      allowed_models: models,
      is_admin: form.isAdmin,
    }
    if (isSuperadmin && form.org) body.organization_id = form.org

    setBusy(true)
    setError(null)
    try {
      onCreated(await createProject(body))
    } catch (err) {
      setError(err.message || 'the project was not created')
      setBusy(false)
    }
  }

  const offered = catalogue ? catalogue.filter((p) => providers.includes(p.name)) : []

  return (
    <form className="pt-create" onSubmit={submit}>
      <h3 className="pt-create-head">New project</h3>

      <div className="pt-create-grid">
        <label className="pt-field">
          <span className="pt-label">Name</span>
          <input className="pt-control" value={form.name} onChange={set('name')} placeholder="Initech Labs" disabled={busy} />
          <span className="pt-hint">The project ID is derived from this and cannot be changed or reused.</span>
        </label>
        {isSuperadmin && (
          <label className="pt-field">
            <span className="pt-label">Organisation</span>
            <select className="pt-control num" value={form.org} onChange={set('org')} disabled={busy}>
              {orgs.map((o) => <option key={o} value={o}>{o}</option>)}
            </select>
          </label>
        )}
        <label className="pt-field">
          <span className="pt-label">Priority</span>
          <select className="pt-control" value={form.priority} onChange={set('priority')} disabled={busy}>
            {PRIORITIES.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
          </select>
        </label>
        <label className="pt-field">
          <span className="pt-label">Requests per minute</span>
          <input className="pt-control num" type="number" min="1" step="1" value={form.rpm} onChange={set('rpm')} placeholder="60" disabled={busy} />
        </label>
        <label className="pt-field">
          <span className="pt-label">Tokens per minute</span>
          <input className="pt-control num" type="number" min="1" step="1" value={form.tpm} onChange={set('tpm')} placeholder="100000" disabled={busy} />
        </label>
        <label className="pt-field">
          <span className="pt-label">Monthly budget (USD)</span>
          <input className="pt-control num" type="number" min="0.01" step="0.01" value={form.budget} onChange={set('budget')} placeholder="50.00" disabled={busy} />
        </label>
      </div>

      <div className="pt-allow">
        <fieldset className="pt-allow-group">
          <legend className="pt-label">Allowed providers</legend>
          {catalogueError && <span className="pt-create-error">{catalogueError}</span>}
          {!catalogue && !catalogueError && <span className="pt-hint">Loading providers…</span>}
          {catalogue?.map((p) => (
            <label key={p.name} className="pt-check">
              <input type="checkbox" checked={providers.includes(p.name)} onChange={() => toggleProvider(p.name)} disabled={busy} />
              <span className="num">{p.name}</span>
            </label>
          ))}
        </fieldset>

        <fieldset className="pt-allow-group">
          <legend className="pt-label">Allowed models</legend>
          {offered.length === 0 && <span className="pt-hint">Tick a provider to choose from its models.</span>}
          {offered.map((p) => (
            <div key={p.name} className="pt-models">
              <span className="pt-models-provider num">{p.name}</span>
              {p.models.map((m) => (
                <label key={`${p.name}/${m}`} className="pt-check">
                  <input type="checkbox" checked={models.includes(m)} onChange={() => toggleModel(m)} disabled={busy} />
                  <span className="num">{m}</span>
                </label>
              ))}
            </div>
          ))}
        </fieldset>
      </div>

      <label className="pt-check pt-create-admin">
        <input type="checkbox" checked={form.isAdmin} onChange={set('isAdmin')} disabled={busy} />
        <span>Admin — can manage projects and read every project's request logs</span>
      </label>

      <div className="pt-create-foot">
        <button type="submit" className="pt-btn pt-btn-primary" disabled={busy}>
          {busy ? 'Creating…' : 'Create project'}
        </button>
        <button type="button" className="pt-btn" onClick={onCancel} disabled={busy}>Cancel</button>
        {error && <span className="pt-create-error" role="alert">{error}</span>}
      </div>
    </form>
  )
}

export default function ProjectTable({ projects, callerProjectId, onChanged }) {
  const [creating, setCreating] = useState(false)
  const [created, setCreated] = useState(null) // { team, api_key, key, warning }

  const onCreated = (result) => {
    setCreating(false)
    setCreated(result)
    onChanged()
  }

  return (
    <div className="pt-wrap">
      <table className="table pt-table">
        <thead>
          <tr>
            <th>Project</th>
            <th>Key</th>
            <th>Priority</th>
            <th className="ta-r">RPM</th>
            <th className="ta-r">TPM</th>
            <th className="ta-r">Monthly budget</th>
            <th className="ta-r">Spent</th>
            <th>Models</th>
            <th>Admin</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {projects.map((p) => (
            <ProjectRow key={p.id} project={p} callerProjectId={callerProjectId} onChanged={onChanged} />
          ))}
        </tbody>
      </table>

      {created && (
        <div className="pt-created">
          <p className="pt-created-note">
            {/* the create/rotate response's project object is still the wire field "team" */}
            Created <span className="pt-name">{created.team.name}</span>{' '}
            <span className="pt-id num">{created.team.id}</span>
          </p>
          <ShowOncePanel result={created} onDismiss={() => setCreated(null)} />
        </div>
      )}

      {creating ? (
        <CreateProjectForm projects={projects} onCreated={onCreated} onCancel={() => setCreating(false)} />
      ) : (
        <div className="pt-create-bar">
          <button type="button" className="pt-btn" onClick={() => { setCreated(null); setCreating(true) }}>
            New project
          </button>
        </div>
      )}
    </div>
  )
}
