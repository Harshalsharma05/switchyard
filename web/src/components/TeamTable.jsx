// Team management (Settings §1–2). A table over Part 1's admin team API: inline
// edit for rate limits and budget, per-team budget reset, and per-team key
// rotate / revoke. No optimistic UI — every action shows a pending state and
// then the server's actual response (DESIGN.md). Every confirmation is inline,
// never a modal. The Key column shows a masked display form for a rotated key
// and the source otherwise — never the key, never the hash.
import { useState } from 'react'
import { patchTeam, resetTeamBudget, rotateTeamKey, revokeTeamKey } from '../api/teams.js'
import { formatUSD } from '../utils/format.js'
import './TeamTable.css'

const COLS = 10

function nonNegative(s) {
  const n = Number(s)
  return s.trim() !== '' && Number.isFinite(n) && n >= 0 ? n : null
}

function keyLabel(key) {
  if (key?.masked) return key.masked
  if (key?.source === 'revoked') return 'revoked'
  return 'config'
}

// The show-once panel: the plaintext key, a copy button, the server's warning,
// and an explicit "not shown again" line. Dismiss collapses it back to masked.
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
    <div className="tt-showonce">
      <div className="tt-showonce-head">New API key — copy it now</div>
      <div className="tt-showonce-keyrow">
        <code className="tt-showonce-key num">{result.api_key}</code>
        <button type="button" className="tt-btn tt-btn-primary" onClick={copy}>
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
      <p className="tt-showonce-warn">
        This key will not be shown again. {result.warning}
      </p>
      <button type="button" className="tt-btn" onClick={onDismiss}>Dismiss</button>
    </div>
  )
}

function TeamRow({ team, callerTeamId, getKey, onChanged }) {
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState({ rpm: '', tpm: '', budget: '' })
  const [busy, setBusy] = useState(null) // 'save' | 'reset' | 'key'
  const [error, setError] = useState(null)
  const [resetPhase, setResetPhase] = useState(null) // null | 'confirm'
  const [keyPhase, setKeyPhase] = useState(null) // null | 'rotate' | 'revoke'
  const [rotated, setRotated] = useState(null) // { api_key, key, warning }

  const rl = team.rate_limits
  const isSelf = team.id === callerTeamId

  const startEdit = () => {
    setForm({ rpm: String(rl.rpm), tpm: String(rl.tpm), budget: String(team.monthly_budget_usd) })
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
    if (budget !== team.monthly_budget_usd) patch.monthly_budget_usd = budget
    if (Object.keys(patch).length === 0) { setEditing(false); return }

    setBusy('save')
    setError(null)
    try {
      await patchTeam(getKey(), team.id, patch)
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
      await resetTeamBudget(getKey(), team.id)
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
      const result = await rotateTeamKey(getKey(), team.id)
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
      await revokeTeamKey(getKey(), team.id)
      onChanged()
    } catch (e) {
      setError(e.message || 'the key was not revoked')
    } finally {
      setBusy(null)
    }
  }

  const field = (name) => (
    <input
      className="tt-input num"
      type="number"
      min="0"
      value={form[name]}
      disabled={busy === 'save'}
      onChange={(e) => setForm((f) => ({ ...f, [name]: e.target.value }))}
    />
  )

  const models = team.allowed_models ?? []

  return (
    <>
      <tr className={editing ? 'tt-row-editing' : ''}>
        <td>
          <span className="tt-name">{team.name}</span>
          <span className="tt-id num">{team.id}</span>
        </td>
        <td className="tt-key">
          <span className="num tt-key-label" title={`key source: ${team.key?.source ?? 'config'}`}>
            {keyLabel(team.key)}
          </span>
          {keyPhase === null && busy !== 'key' && (
            <span className="tt-key-actions">
              <button type="button" className="tt-btn tt-btn-sm" onClick={() => setKeyPhase('rotate')} disabled={busy != null}>
                Rotate
              </button>
              <button
                type="button"
                className="tt-btn tt-btn-sm"
                onClick={() => setKeyPhase('revoke')}
                disabled={busy != null || isSelf}
                title={isSelf ? 'You cannot revoke the key you are signed in with' : undefined}
              >
                Revoke
              </button>
            </span>
          )}
          {busy === 'key' && <span className="tt-muted">Working…</span>}
          {keyPhase === 'rotate' && (
            <span className="tt-confirm">
              Rotate this key? The current key stops working immediately.
              <button type="button" className="tt-btn tt-btn-sm tt-btn-primary" onClick={rotate}>Rotate</button>
              <button type="button" className="tt-btn tt-btn-sm" onClick={() => setKeyPhase(null)}>Cancel</button>
            </span>
          )}
          {keyPhase === 'revoke' && (
            <span className="tt-confirm">
              Revoke this key? {team.name} will have no working key until you rotate one.
              <button type="button" className="tt-btn tt-btn-sm tt-btn-danger" onClick={revoke}>Revoke</button>
              <button type="button" className="tt-btn tt-btn-sm" onClick={() => setKeyPhase(null)}>Cancel</button>
            </span>
          )}
        </td>
        <td>{team.priority}</td>
        <td className="num ta-r">{editing ? field('rpm') : rl.rpm.toLocaleString()}</td>
        <td className="num ta-r">{editing ? field('tpm') : rl.tpm.toLocaleString()}</td>
        <td className="num ta-r">{editing ? field('budget') : formatUSD(team.monthly_budget_usd)}</td>
        <td className="num ta-r">{team.spent_usd == null ? '—' : formatUSD(team.spent_usd)}</td>
        <td className="num" title={models.join(', ')}>
          {models.length} model{models.length === 1 ? '' : 's'}
        </td>
        <td>
          {team.is_admin
            ? <span className="tt-admin">Admin</span>
            : <span className="tt-muted">·</span>}
        </td>
        <td className="tt-actions">
          {editing ? (
            <>
              <button type="button" className="tt-btn tt-btn-primary" onClick={save} disabled={busy === 'save'}>
                {busy === 'save' ? 'Saving…' : 'Save'}
              </button>
              <button type="button" className="tt-btn" onClick={cancel} disabled={busy === 'save'}>
                Cancel
              </button>
            </>
          ) : resetPhase === 'confirm' ? (
            <span className="tt-confirm">
              Reset {team.name}'s spend to $0.00?
              <button type="button" className="tt-btn tt-btn-sm tt-btn-danger" onClick={reset}>Reset</button>
              <button type="button" className="tt-btn tt-btn-sm" onClick={() => setResetPhase(null)}>Cancel</button>
            </span>
          ) : (
            <>
              <button type="button" className="tt-btn" onClick={startEdit} disabled={busy != null}>
                Edit
              </button>
              <button type="button" className="tt-btn" onClick={() => setResetPhase('confirm')} disabled={busy != null}>
                {busy === 'reset' ? 'Resetting…' : 'Reset budget'}
              </button>
            </>
          )}
        </td>
      </tr>
      {rotated && (
        <tr className="tt-showonce-row">
          <td colSpan={COLS}>
            <ShowOncePanel result={rotated} onDismiss={() => setRotated(null)} />
          </td>
        </tr>
      )}
      {error && (
        <tr className="tt-error-row">
          <td colSpan={COLS}>{error}</td>
        </tr>
      )}
    </>
  )
}

export default function TeamTable({ teams, callerTeamId, getKey, onChanged }) {
  return (
    <div className="tt-wrap">
      <table className="table tt-table">
        <thead>
          <tr>
            <th>Team</th>
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
          {teams.map((t) => (
            <TeamRow key={t.id} team={t} callerTeamId={callerTeamId} getKey={getKey} onChanged={onChanged} />
          ))}
        </tbody>
      </table>
      <p className="tt-create-note">
        Teams are stored in Postgres. Editing limits and rotating keys here is durable;
        creating and deleting teams is not available yet.
      </p>
    </div>
  )
}
