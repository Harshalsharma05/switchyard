// Feature carousel. Manual only — no timer, no auto-advance.
//
// That is the point: the typed tagline is the one thing moving on its own, so
// the features sit still until someone asks for the next one.
import { useRef, useState } from 'react'
import { FEATURES } from './features.js'
import FeatureIcon from './FeatureIcons.jsx'

export default function FeatureCarousel() {
  const [active, setActive] = useState(0)
  const dotsRef = useRef([])
  // Mirrors active so a burst of arrow presses cannot read a stale value from
  // the render closure and drop a step.
  const activeRef = useRef(0)

  const select = (i) => {
    activeRef.current = i
    setActive(i)
  }

  const step = (delta) => {
    const i = (activeRef.current + delta + FEATURES.length) % FEATURES.length
    select(i)
    dotsRef.current[i]?.focus()
  }

  const onKeyDown = (e) => {
    if (e.key === 'ArrowRight') { e.preventDefault(); step(1) }
    if (e.key === 'ArrowLeft') { e.preventDefault(); step(-1) }
  }

  const feature = FEATURES[active]

  return (
    <>
      {/* Decorative: the dots below carry the accessible names. Re-keyed on
          active so the fade-and-drift replays for each card. */}
      <div className="auth-feature" aria-hidden="true">
        <article className="auth-feature-card" key={active}>
          <span className="auth-feature-icon"><FeatureIcon name={feature.icon} /></span>
          <div>
            <h3>{feature.title}</h3>
            <p>{feature.body}</p>
          </div>
        </article>
      </div>

      <div className="auth-dots" role="group" aria-label="Product highlights" onKeyDown={onKeyDown}>
        {FEATURES.map((f, i) => (
          <button
            key={f.icon}
            ref={(el) => { dotsRef.current[i] = el }}
            type="button"
            className={`auth-dot${i === active ? ' is-active' : ''}`}
            aria-label={`Feature ${i + 1} of ${FEATURES.length}`}
            aria-current={i === active}
            onClick={() => select(i)}
          />
        ))}
      </div>
    </>
  )
}
