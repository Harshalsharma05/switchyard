// Recent Playground prompts for the current session — in memory only, gone on
// reload. A convenience for re-running a prompt, deliberately separate from the
// durable Request Logs, which never store prompt text at all.
let entries = []
let nextId = 1
const listeners = new Set()
const LIMIT = 20

export function pushHistory({ prompt, model, streaming }) {
  // Collapse an immediate repeat so re-running the same prompt does not stack
  // two identical rows.
  if (entries[0]?.prompt === prompt && entries[0]?.model === model) return
  entries = [{ id: nextId++, prompt, model, streaming, at: new Date().toISOString() }, ...entries].slice(0, LIMIT)
  listeners.forEach((l) => l())
}

export function subscribeHistory(listener) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function getHistory() {
  return entries
}
