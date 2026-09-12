// Carousel glyphs. Inline SVG in the same grammar as the rail icons —
// 18px, 1.5px stroke, currentColor so the card's accent drives them.
const base = {
  width: 18, height: 18, viewBox: '0 0 24 24', fill: 'none',
  stroke: 'currentColor', strokeWidth: 1.5, strokeLinecap: 'round', strokeLinejoin: 'round',
}

const Route = () => (
  <svg {...base}>
    <path d="M3 12h4" />
    <path d="M7 12c3.5 0 3.5-6 7-6" />
    <path d="M7 12c3.5 0 3.5 6 7 6" />
    <circle cx="16.5" cy="6" r="2" />
    <circle cx="16.5" cy="18" r="2" />
  </svg>
)

const Receipt = () => (
  <svg {...base}>
    <path d="M5 3h14v18l-2.3-1.4L14.4 21 12 19.6 9.6 21 7.3 19.6 5 21z" />
    <line x1="9" y1="8.5" x2="15" y2="8.5" />
    <line x1="9" y1="12.5" x2="15" y2="12.5" />
  </svg>
)

const Shield = () => (
  <svg {...base}>
    <path d="M12 3l7 3v5c0 5-3 8.2-7 10-4-1.8-7-5-7-10V6z" />
    <path d="M12 8v8" />
    <path d="M14 10.1a2 2 0 0 0-2-1.2c-1.1 0-2 .6-2 1.6s.9 1.3 2 1.6 2 .6 2 1.6-.9 1.6-2 1.6a2 2 0 0 1-2-1.2" />
  </svg>
)

const Bolt = () => (
  <svg {...base}>
    <path d="M13 3L5.5 14H11l-1 7 7.5-11H12z" />
  </svg>
)

const Plug = () => (
  <svg {...base}>
    <path d="M17.5 6.5 21 3" />
    <path d="M7 17 3.5 20.5" />
    <path d="M11.2 5.4l7.4 7.4-3 3a3.4 3.4 0 0 1-4.8 0l-2.6-2.6a3.4 3.4 0 0 1 0-4.8z" />
  </svg>
)

const ICONS = { route: Route, receipt: Receipt, shield: Shield, bolt: Bolt, plug: Plug }

export default function FeatureIcon({ name }) {
  const Glyph = ICONS[name]
  return Glyph ? <Glyph /> : null
}
