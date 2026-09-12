// The typed tagline — the only thing on this page that animates on its own.
//
// Mono is deliberate: it makes the cursor read as a terminal caret rather than
// a marketing gimmick, and it matches DESIGN.md's rule that technical strings
// are mono.
import { useEffect, useRef, useState } from 'react'
import { TAGLINES } from './features.js'
import { usePrefersReducedMotion } from './usePrefersReducedMotion.js'

const CHAR_MS = 45      // per character
const JITTER_MS = 15    // ±, so the typing is not mechanical
const HOLD_MS = 2200    // complete line sits still
const FADE_MS = 350     // fade out
const GAP_MS = 200      // blank beat before the next line
const START_MS = 400    // first line waits for the page to settle

export default function TypedTagline() {
  const reduced = usePrefersReducedMotion()
  const [index, setIndex] = useState(0)
  const [text, setText] = useState('')
  const [fading, setFading] = useState(false)
  const firstRun = useRef(true)

  useEffect(() => {
    if (reduced) return undefined

    let cancelled = false
    const timers = []
    const after = (ms, fn) => {
      timers.push(setTimeout(() => { if (!cancelled) fn() }, ms))
    }

    const line = TAGLINES[index]

    const typeTo = (n) => {
      setText(line.slice(0, n))
      if (n < line.length) {
        after(CHAR_MS + (Math.random() * 2 - 1) * JITTER_MS, () => typeTo(n + 1))
        return
      }
      // Complete: hold, fade with the cursor, then hand over to the next line.
      after(HOLD_MS, () => setFading(true))
      after(HOLD_MS + FADE_MS + GAP_MS, () => setIndex((i) => (i + 1) % TAGLINES.length))
    }

    // The reset happens here rather than in the effect body: the previous line
    // is still faded out at this point, so nothing flashes.
    after(firstRun.current ? START_MS : 0, () => { setFading(false); typeTo(1) })
    firstRun.current = false

    return () => { cancelled = true; timers.forEach(clearTimeout) }
  }, [index, reduced])

  // One line, no cursor, no loop.
  if (reduced) {
    return <p className="auth-tagline">{TAGLINES[0]}</p>
  }

  return (
    <p className={`auth-tagline${fading ? ' is-fading' : ''}`}>
      {text}
      <span className="auth-caret" />
    </p>
  )
}
