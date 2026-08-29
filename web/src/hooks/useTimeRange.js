// The shared time range the top bar's selector sets and range-aware screens
// read. Context rather than a module store because it is ordinary UI state
// scoped to the signed-in shell.
import { createContext, useContext } from 'react'

export const RANGES = ['1h', '24h', '7d']

export const TimeRangeContext = createContext(null)

export function useTimeRange() {
  const ctx = useContext(TimeRangeContext)
  if (!ctx) throw new Error('useTimeRange must be used within TimeRangeProvider')
  return ctx
}
