// The auth context and its hook. Kept separate from the provider component so
// the provider file exports only a component (React Fast Refresh requires it).
import { createContext, useContext } from 'react'

export const STORAGE_KEY = 'switchyard.teamKey'

export const AuthContext = createContext(null)

export function useAuth() {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
