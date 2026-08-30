// Team management (Step 6.4): a table over Part 1's admin team API. Inline
// edit for rate limits and budget, a per-team budget reset. No optimistic UI —
// a change shows a pending state and then the server's actual response
// (DESIGN.md). Key metadata is a short one-way fingerprint, never the key.
import { useState } from 'react'
import { patchTeam, resetTeamBudget } from '../api/teams.js'
import { formatUSD } from '../utils/format.js'
import './TeamTable.css'

function nonNegative(s) {
  const n = Number(s)
  return s.trim() !== '' && Number.isFinite(n) && n >= 0 ? n : null
}

function TeamRow({ team, getKey, onChanged }) {
  const [editing, setEditing] = useState(false)
  const [form, setForm] = useState({ rpm: '', tpm: '', budget: '' })
  const [busy, setBusy] = useState(null) // 'save' | 'reset'
  const [error, setError] = useState(null)

  const rl = team.rate_limits

  const startEdit = () => {
    setForm({ rpm: String(rl.rpm), tpm: String(rl.tpm), budget: String(team.monthly_budget_usd) })
    setError(null)
    setEditing(true)
  }
  const cancel = () => {
    setEditing(false)
    setError(null)
  }

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
    if (Object.keys(patch).length === 0) {
      setEditing(false)
      return
    }

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
    if (!window.confirm(`Reset ${team.name}'s spend for this month to $0.00?`)) return
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

  const field = (key) => (
    <input
      className="tt-input num"
      type="number"
      min="0"
      value={form[key]}
      disabled={busy === 'save'}
      onChange={(e) => setForm((f) => ({ ...f, [key]: e.target.value }))}
    />
  )

  return (
    <>
      <tr className={editing ? 'tt-row-editing' : ''}>
        <td>
          <span className="tt-name">{team.name}</span>
          <span className="tt-id num">{team.id}</span>
        </td>
        <td className="num tt-fingerprint" title="first 12 hex of the key digest">
          {team.key_fingerprint ? `#${team.key_fingerprint}` : '—'}
        </td>
        <td>{team.priority}</td>
        <td className="num ta-r">{editing ? field('rpm') : rl.rpm.toLocaleString()}</td>
        <td className="num ta-r">{editing ? field('tpm') : rl.tpm.toLocaleString()}</td>
        <td className="num ta-r">{editing ? field('budget') : formatUSD(team.monthly_budget_usd)}</td>
        <td className="num ta-r">{team.spent_usd == null ? '—' : formatUSD(team.spent_usd)}</td>
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
          ) : (
            <>
              <button type="button" className="tt-btn" onClick={startEdit} disabled={busy != null}>
                Edit
              </button>
              <button type="button" className="tt-btn" onClick={reset} disabled={busy != null}>
                {busy === 'reset' ? 'Resetting…' : 'Reset budget'}
              </button>
            </>
          )}
        </td>
      </tr>
      {error && (
        <tr className="tt-error-row">
          <td colSpan={8}>{error}</td>
        </tr>
      )}
    </>
  )
}

export default function TeamTable({ teams, getKey, onChanged }) {
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
            <th />
          </tr>
        </thead>
        <tbody>
          {teams.map((t) => (
            <TeamRow key={t.id} team={t} getKey={getKey} onChanged={onChanged} />
          ))}
        </tbody>
      </table>
    </div>
  )
}
