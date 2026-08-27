# DESIGN.md — SwitchYard console

The visual and interaction authority for `web/`. `PART2_PLAN.md` specifies what each screen does and what data backs it; this file specifies how all of it looks and behaves.

When the two disagree on anything visual, this file wins. When a component isn't described here, derive it from the principles below rather than inventing a new pattern.

---

## Principles

**Restraint is the aesthetic.** This is an operations console, not a marketing page. Every visual element must earn its place by carrying information. No decorative gradients, no glows, no 3D renders, no illustrations. If removing something loses no information, remove it.

**Data is the interface.** Numbers, states, and time series are the content. Chrome exists to frame them and then get out of the way. Charts are drawn with as little ink as possible around the data itself.

**One accent, used sparingly.** Colour means something here — it encodes status. Spending it on decoration destroys its ability to signal. A screen where everything is coloured is a screen where nothing stands out.

**Density without crowding.** An operator scanning this dashboard should take in a lot at once. That means tight, consistent spacing and small type — not large cards with a single number floating in the middle.

**Live by default.** Numbers tick. Feeds scroll. Status changes animate their transition. A console that requires a refresh reads as a screenshot.

---

## Foundations

### Colour

Dark-first. A light theme is explicitly out of scope for Part 2 — build one theme well rather than two poorly.

Define every value as a CSS custom property on `:root` in `web/src/styles/tokens.css`. Never hardcode a hex value in a component.

**Surfaces**

| Token | Value | Use |
|---|---|---|
| `--bg-base` | `#0B0D0F` | Page background |
| `--bg-surface` | `#131619` | Cards, panels |
| `--bg-raised` | `#1A1E22` | Hover states, nested panels, table header |
| `--bg-inset` | `#0E1113` | Inputs, code blocks, inset wells |

**Borders**

| Token | Value | Use |
|---|---|---|
| `--border` | `#22272C` | Default hairline — the workhorse |
| `--border-strong` | `#2E353B` | Hover, active, emphasis |

Borders are `1px`. Never thicker except for the 2px left-accent on an active nav item.

**Text**

| Token | Value | Use |
|---|---|---|
| `--text-primary` | `#E6E9EC` | Values, headings, table cells |
| `--text-secondary` | `#9BA4AD` | Labels, supporting copy |
| `--text-muted` | `#5F6A73` | Timestamps, units, hints, disabled |

**Status — the only colours that carry meaning**

| Token | Value | Meaning |
|---|---|---|
| `--status-healthy` | `#3FB950` | Healthy, closed breaker, 2xx, under budget |
| `--status-warn` | `#D29922` | Degraded, half-open, 429, 80% budget |
| `--status-error` | `#F85149` | Down, open breaker, 5xx, 402, over budget |
| `--status-info` | `#4C8DFF` | Cache hit, fallback served, routed-down, neutral emphasis |

Each status colour also gets a `-bg` variant at ~12% opacity over the surface, for pill and badge backgrounds. Text on a status background uses the status colour itself, never white or grey.

**Accent**

`--accent: #4C8DFF`, same as `--status-info`. Used for: the active nav item, primary buttons, focused inputs, links, and the primary chart series. One accent-filled button per view, maximum.

### Typography

Two families only.

```css
--font-sans: 'Inter', system-ui, -apple-system, sans-serif;
--font-mono: 'JetBrains Mono', ui-monospace, 'SF Mono', monospace;
```

Load Inter and JetBrains Mono from a font CDN or bundle them locally — either is fine, decide once.

| Role | Size | Weight | Family |
|---|---|---|---|
| Page title | 20px | 500 | sans |
| Section heading | 15px | 500 | sans |
| KPI value | 28px | 500 | sans, tabular figures |
| Body / table cell | 13px | 400 | sans |
| Label / caption | 12px | 400 | sans |
| Micro (units, timestamps) | 11px | 400 | sans |
| IDs, models, keys, code, latency, cost | 12–13px | 400 | **mono** |

**Weights: 400 and 500 only.** Never 600 or 700 — heavy weights against a dark background read as noise.

**Sentence case everywhere.** Never Title Case, never ALL CAPS — including table headers and chart axis labels. Uppercase micro-labels are a common dashboard tic and they look dated.

**Every number is monospace with tabular figures** (`font-variant-numeric: tabular-nums`). This is not optional. Proportional digits make a live-updating KPI jitter horizontally as its value changes, and misalign columns in a table. This single rule does more for perceived polish than anything else in this file.

### Spacing and radius

A 4px scale: `4 · 8 · 12 · 16 · 24 · 32 · 48`. Nothing off-scale.

- Card padding: `16px`
- Gap between cards in a grid: `12px`
- Gap between page sections: `24px`
- Table cell padding: `10px 12px`

Radius: `6px` for controls, inputs, pills, and buttons. `10px` for cards and panels. Nothing fully rounded except status dots.

No shadows anywhere. Elevation is communicated by surface colour and border, not by blur.

---

## Layout

### Shell

A fixed left rail, `56px` wide, icon-only, with a tooltip on hover. The active item gets a `2px` left accent bar and `--bg-raised`. Rail contains, in order: Overview, Playground, Live Ops, Request Logs, Usage & Cost, then a divider, then Settings. Admin-only items are simply absent for non-admins, not disabled.

Top bar, `52px`: product name at left, then the current team name and an `Admin` pill if applicable. At right: a time-range selector (where the screen has one), a live-status indicator, and the key/session control.

The live indicator is a small dot plus a word — green `Live` when updating normally, amber `Reconnecting` on backoff, red `Disconnected` when the gateway is unreachable. It is always visible. It is the user's proof that what they're looking at is current.

Content area: `max-width: 1440px`, centred, `24px` padding.

### Grid

Twelve columns, `12px` gutter. Common arrangements:

- KPI row: 5 equal cards across the full width
- Two-up: 8 / 4 split (main chart + side panel)
- Full width: tables, feeds

Below `1100px`, collapse to a single column and stack. Below `768px` is out of scope — this is a desktop console and pretending otherwise produces a bad version of both.

---

## Components

### Card

`--bg-surface`, `1px solid --border`, `10px` radius, `16px` padding.

Header row when present: heading at `15px/500` on the left, an optional action (link, filter, menu) at the right, `12px` below the header before content. No divider line under the header — the spacing is enough.

### KPI card

The most-used component. Structure, top to bottom:

1. Label — `12px`, `--text-secondary`
2. Value — `28px/500`, mono, tabular figures, `--text-primary`
3. Delta or context — `11px`, `--text-muted`, with a `↑`/`↓` and status colour when it represents a change

No icons. No sparkline inside the KPI card — if a trend matters, it belongs in a proper chart below, at a readable size.

When a KPI has no data yet (cache hit rate before Phase 7), show `—` in `--text-muted` with the label `Not yet enabled` beneath. Never `0`, never a fabricated number. An empty state that looks like real data is worse than no data.

### Status pill

Dot plus label. Dot is `6px`, fully round, in the status colour. Label is `12px` in the status colour. Background is the status colour at 12% opacity, `6px` radius, `3px 8px` padding.

Used for provider health, breaker state, HTTP status, budget state. Same component everywhere — one visual grammar for "what state is this in."

### Table

`--bg-surface` container. Header row on `--bg-raised`, `11px`, `--text-secondary`, sentence case, sticky on scroll.

Rows: `1px` bottom border in `--border`, `10px 12px` cell padding, `13px` text. Hover raises the row to `--bg-raised`. Rows are not cards — no radius, no gaps, no individual borders. Dense lists are bordered rows.

Numeric and identifier columns are mono and right-aligned. Text columns are left-aligned. Status columns use the status pill.

Long identifiers (request IDs, trace IDs) truncate with a middle ellipsis and reveal in full on hover or in the row detail. Never wrap them.

Row click opens a detail panel — a right-side drawer, not a modal. Drawers preserve the context of the list behind them.

### Live feed

A table variant. New rows enter at the top with a brief `--bg-raised` flash that fades over ~600ms, then settle. No slide-in animation, no layout shift — the flash alone is enough to draw the eye.

Cap at a fixed row count and drop from the bottom. Pause insertion on hover so a user can read a row without it scrolling away.

### Buttons

Default is secondary: transparent background, `1px solid --border-strong`, `--text-primary`, `6px` radius, `32px` height, `13px` label. Hover fills to `--bg-raised`.

Primary (accent fill, `--text-primary` on accent) is reserved for the single most important action on a screen — Playground's Send, and nothing else by default.

Destructive actions (clear all chaos, reset budget) use `--status-error` as a border and text colour, not as a fill. Filled red buttons read as alarming; the action is deliberate, not an emergency.

Never disable a button silently. If an action is unavailable, keep it enabled and explain on click, or state the reason inline beneath it.

### Inputs

`--bg-inset` background, `1px solid --border`, `6px` radius, `32px` height (textareas grow), `13px`. Focus: border becomes `--accent` plus a `2px` accent ring at low opacity. No other focus treatment.

Placeholders are real examples, not instructions — `Summarize this document`, not `Enter your prompt here`.

### Empty, loading, and error states

Every data surface needs all three. Build them once as shared components.

- **Loading** — skeleton blocks in `--bg-raised` matching the shape of the content, with a subtle pulse. Never a centred spinner; never a layout that jumps when data arrives.
- **Empty** — one line of `--text-secondary` naming what would appear here, and an action if one makes sense. No illustration, no apology, no "Nothing here yet."
- **Error** — one line stating what failed and what to do, plus a retry control. Never a raw error string, never a stack trace.

---

## Charts

Recharts, and the goal is that it stops looking like Recharts.

### Defaults to override

Recharts' defaults are wrong for this design. Set all of these on every chart:

- **Remove the cartesian grid's vertical lines.** Horizontal only, `stroke: --border`, `strokeDasharray: 3 3`.
- **Remove axis lines and tick lines.** `axisLine={false} tickLine={false}` on both axes. The gridlines already imply the axes.
- **Ticks**: `11px`, `--text-muted`, mono for numeric axes.
- **No chart-level title inside the chart.** The card header is the title.
- **No legend when there is one series.** With multiple series, a compact top-right legend at `11px`, no legend symbols larger than `8px`.
- **Margins**: tight. `{ top: 8, right: 8, bottom: 0, left: 0 }` and rely on the card's padding.

### Series colour

One series → `--accent`. Two → accent plus `--text-muted`. Three or more → accent, then a restrained sequence you define once in `chartColors.js` and reuse everywhere.

When series encode status (success/warn/error breakdowns), use the status colours and nothing else. Never colour a series for variety.

### Line and area

Lines: `1.5px`, no dots on the line itself. Dots only on hover, `3px`, filled with the series colour.

Area fills: the series colour at 8% opacity, flat — a solid low-opacity fill, never a vertical gradient. Gradients under area charts are the single most common way a dashboard reads as generic.

Curve: `monotone`. Never `basis` (it distorts values) and never `linear` for time series.

### Bar

`--accent` at full opacity, `2px` top radius, `maxBarSize={28}`. Gaps between bars wider than the bars look sparse; `barCategoryGap="20%"` is a reasonable start.

### Tooltip

Always custom. Recharts' default tooltip does not match anything else in this app.

`--bg-raised` background, `1px solid --border-strong`, `8px` radius, `8px 10px` padding, `12px` text. Label line in `--text-secondary`, values in mono `--text-primary`. Cursor line is a `1px --border-strong` vertical, no shaded band.

### Axis and formatting

- Time axes format by range: `HH:mm` within a day, `MMM d` beyond it. Never show a raw timestamp.
- Numbers abbreviate above 10,000 (`12.4k`), currency to two decimals, latency in `ms` with the unit in `--text-muted` after the value.
- Y-axis starts at zero for counts and costs. For latency percentiles, a non-zero floor is acceptable and often clearer — but label it so it isn't misread.

### Sizing

Charts are `180–240px` tall in a KPI-adjacent context, `280–320px` as a primary panel. Never taller than `360px` — a tall chart in a console pushes everything else below the fold for no gain.

---

## Screen notes

Layout only. Data and behaviour are in `PART2_PLAN.md`.

**Overview** — KPI row of 5 across the top. Below: an 8/4 split with the traffic and latency chart on the left and a stacked provider-health and breaker-state panel on the right. Full-width live request feed at the bottom. The Grafana link is a small `↗` text link in the top-right of the chart card, not a button.

**Playground** — a 7/5 split. Left: prompt textarea, model selector, streaming toggle, Send. The response renders below the input in the same column, streaming in. Right: the metadata panel as a definition list — label in `--text-secondary`, value in mono — plus session history beneath it. Error responses (429, 402, 503) render in the response area as a bordered block using the relevant status colour on its border and heading, with the structured detail formatted, never as raw JSON.

**Live Ops** — provider cards in a row across the top, each with health, latency, error rate, and its chaos controls. Breaker visualisations below, one per provider+model, rendering the three-state machine with the current state highlighted and the others dimmed. Load simulator in its own full-width card at the bottom with its live readout as a compact KPI strip. Chaos-active state must be unmissable: a persistent banner at the top of the content area in `--status-warn`, present on every screen, not just this one.

**Request Logs** — filter bar pinned above the table: selects and a time range, with an active-filter count and a clear-all. Table fills the rest. Row click opens the right drawer at `480px` with the full record, the routing decision, the fallback chain if any, and the Jaeger link as a labelled `↗` action.

**Usage & Cost** — team spend cards at the top, each a KPI card with a thin progress bar beneath the value (`4px`, `--border` track, status colour fill by threshold). Cost trend chart below at full width. Attribution panels — cache savings, routing savings, fallback cost — as a three-up row beneath. Team management, admin only, as a table at the bottom with inline edit.

---

## Interaction

**Motion is functional only.** Three durations: `120ms` for hover and focus, `200ms` for state transitions and drawer entry, `600ms` for the live-feed row flash. Ease `cubic-bezier(0.2, 0, 0, 1)`. Nothing else animates.

Respect `prefers-reduced-motion` — disable the feed flash and drawer slide, keep the state change instant.

**Live updates never move things under the cursor.** Numbers update in place. New feed rows pause on hover. A filter or sort a user set is never reset by an update.

**Optimistic UI is not appropriate here.** When an admin changes a rate limit or resets a breaker, show a pending state and reflect the server's actual response. This is a control plane — showing a change that didn't apply is worse than a brief delay.

**Every destructive or system-affecting action confirms**: clearing chaos, resetting a budget, forcing a breaker closed. Inline confirmation in place, not a modal.

---

## Accessibility

Not optional, and cheap to get right if done from the start.

- Text contrast at least 4.5:1 against its surface; `--text-muted` is the floor and must be checked, not assumed.
- **Status is never colour alone.** Every status pill has a text label beside its dot. A red dot with no word fails for a colourblind user and reads as ambiguous for everyone else.
- Every interactive element is keyboard reachable with a visible focus ring — the same accent ring as input focus.
- Drawers trap focus and close on `Escape`.
- Live regions (`aria-live="polite"`) on the connection indicator and on chaos-active state, so a screen reader announces them.
- Charts get an accessible summary; the underlying data is reachable in a table somewhere in the app for every chart shown.

---

## Anti-patterns

Explicitly forbidden, because they are what makes a dashboard look templated:

- Gradient fills under area charts, or gradient backgrounds anywhere
- Glow, neon, or coloured shadow effects
- Large icons inside KPI cards
- Sparklines embedded in metric cards
- Uppercase micro-labels
- Proportional-figure numbers in tables or live values
- Rounded "pill" cards in dense lists
- Emoji as status indicators
- More than one accent-filled button per view
- A spinner centred in an empty card
- Fabricated placeholder data that looks real
