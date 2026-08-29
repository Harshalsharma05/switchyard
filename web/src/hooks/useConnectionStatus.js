// Connection status for the top-bar live indicator, driven by whether the
// polling hooks are actually getting answers from the gateway.
import { useSyncExternalStore } from 'react'
import { getConnectionStatus, subscribeConnection } from './connectionStore.js'

// 'live' | 'reconnecting' | 'disconnected' — the three states DESIGN.md names.
export function useConnectionStatus() {
  return useSyncExternalStore(subscribeConnection, getConnectionStatus, () => 'live')
}
