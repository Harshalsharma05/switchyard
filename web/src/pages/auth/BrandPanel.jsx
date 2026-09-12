// The left half of the auth page: a pinned wordmark plus one vertically
// centred content column.
//
// The headline is static and stays that way — the tagline is the only element
// on this page that animates on its own.
//
// aria-hidden covers the decorative zones so a screen reader reaches the form
// without walking past a carousel. It deliberately does NOT wrap the carousel
// dots: those are real controls, and focusable elements inside an aria-hidden
// subtree are an accessibility bug, not a nicety.

import TypedTagline from './TypedTagline.jsx'
import FeatureCarousel from './FeatureCarousel.jsx'

function LogoMark() {
  return (
    <svg
      width="22" height="22" viewBox="0 0 24 24" fill="none"
      stroke="var(--accent)" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"
    >
      <path d="M3 12h5" />
      <path d="M8 12 15 5" />
      <path d="M8 12l7 7" />
      <circle cx="18" cy="5" r="2.2" />
      <circle cx="18" cy="19" r="2.2" />
      <circle cx="3" cy="12" r="1.2" fill="var(--accent)" />
    </svg>
  )
}

export default function BrandPanel() {
  return (
    <aside className="auth-brand">
      {/* Pinned, not part of the flex distribution: otherwise a tall viewport
          flings the zones apart and pushes the carousel out of view. */}
      <div className="auth-brand-top" aria-hidden="true">
        <LogoMark />
        <span>SwitchYard</span>
      </div>

      <div className="auth-brand-content">
        <div className="auth-brand-mid" aria-hidden="true">
          <p className="auth-eyebrow">Welcome to SwitchYard</p>
          <h2 className="auth-headline">One endpoint for every model you use.</h2>
          <TypedTagline />
        </div>

        <div className="auth-brand-bottom">
          <FeatureCarousel />
        </div>
      </div>
    </aside>
  )
}
