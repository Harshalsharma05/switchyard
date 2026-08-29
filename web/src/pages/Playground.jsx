// Playground — a live prompt against POST /v1/chat/completions with a full
// metadata panel and first-class error rendering. Built in Phase 3.
import { EmptyState } from '../components/states.jsx'

export default function Playground() {
  return (
    <>
      <h1 className="page-title">Playground</h1>
      <EmptyState>
        A prompt input, model selector, and streaming response with per-request
        metadata will be built here in Phase 3.
      </EmptyState>
    </>
  )
}
