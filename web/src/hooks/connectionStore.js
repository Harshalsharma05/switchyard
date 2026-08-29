// A tiny external store for the top bar's live indicator. The polling hooks
// write to it and LiveIndicator reads it, which avoids threading connection
// state from every page up through the shell.
let status = 'live'
const listeners = new Set()

// Each poller reports independently; the worst status any of them is in wins,
// so one failing feed is not hidden by another that is still succeeding.
const reporters = new Map()

const RANK = { live: 0, reconnecting: 1, disconnected: 2 }

function recompute() {
  let worst = 'live'
  for (const s of reporters.values()) {
    if (RANK[s] > RANK[worst]) worst = s
  }
  if (worst !== status) {
    status = worst
    listeners.forEach((l) => l())
  }
}

export function reportConnection(id, next) {
  reporters.set(id, next)
  recompute()
}

export function clearConnection(id) {
  reporters.delete(id)
  recompute()
}

export function subscribeConnection(listener) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function getConnectionStatus() {
  return status
}
