# PART 2 — Implementation Plan

SwitchYard, Part 2. The `part1-complete` is tagged and the gateway runs end to end via `docker compose up`.

`PART1_PLAN.md` remains the reference for what already exists. `CLAUDE.md` still governs how work is done — every rule in it applies here unchanged. **All frontend visual specification — theme, palette, typography, spacing, component anatomy, chart styling, interaction and motion — lives in `DESIGN.md`, not this file.** This file specifies what each screen *does*, what data backs it, and how you know it's finished.

---

## Ground rules for Part 2

1. **Part 1's guarantees are frozen.** Nothing in Part 2 may change the behaviour of `internal/ratelimit`, `internal/resilience`, or `internal/health`. Read from them freely; do not modify them. If a Part 2 feature seems to require a change there, stop and raise it — that is a design problem, not a licence.

2. **The gateway must never be the reason a request fails** — still load-bearing. Postgres, the embedding model, the cache, the classifier, and the quality worker are all new dependencies. Each one gets an explicit, documented fail-open or fail-closed decision, and every one of them fails *open* unless there is a written reason otherwise.

3. **Overhead p95 < 10ms still holds.** Part 1 measured 3.11ms. Cache lookup and routing classification both sit on the hot path. If either pushes p95 past 10ms, that is a blocker, not a footnote — profile before proceeding.

4. **Placeholder before real.** Screens ship with cache/routing/quality widgets visible and empty-stated from Phase 2 onward. Wiring them up in Phases 7–9 must be a data change, not a UI rebuild.

5. **The frontend never talks to Prometheus, Postgres, or a provider directly.** Every screen reads from a gateway endpoint. No credentials in the browser beyond the team API key the user is authenticated with.

6. **Every number the UI displays must be traceable to a real source.** No hardcoded demo values that survive past the phase that introduced them. If a metric isn't available yet, the UI shows an empty state — it does not show a plausible-looking fake.

7. **One phase per session.** `/clear` between phases, run the checklist, commit, tag, and append to `DECISIONS.md` before moving on.

---

## Repo additions

```
web/                        Vite + React (JSX) frontend
  src/
    api/                    fetch wrappers, one module per gateway resource
    components/             shared UI primitives (see DESIGN.md)
    pages/                  one file per screen
    hooks/                  polling, SSE, auth/session
internal/logstore/          Postgres request-log writer + query layer
internal/cache/             semantic cache (Phase 7)
internal/router/            complexity classifier + cost-aware routing (Phase 8)
internal/quality/           async verification worker (Phase 9)
internal/summary/           Prometheus query layer for dashboard endpoints
migrations/                 SQL migrations for the request-log schema
```

`internal/logstore/` is written to from the proxy's post-response path and read from by the admin API. It must not be importable from `internal/provider/` or the resilience packages.

---

## Phase 0 — Prerequisites (manual)

Do these yourself before starting Phase 1. Nothing here is code.

### 0.1 — Postgres

- Add a `postgres` service to `deploy/docker-compose.yml` (Postgres 16, named volume for persistence, healthcheck so the gateway's `depends_on` can wait on it).
- Decide and record: database name, user, and how the password reaches the gateway (env var, same pattern as provider API keys — never in the YAML).
- Confirm `docker compose up -d postgres` comes up healthy and you can connect with `psql` or a GUI client.

### 0.2 — Node toolchain

- Install Node 20+ and confirm `node -v` / `npm -v`.
- If you moved to WSL2, install Node *inside* WSL2, not on Windows — same reasoning as Go.

### 0.3 — Scaffold the frontend shell

```bash
npm create vite@latest web -- --template react
cd web && npm install
```

Then add the dependencies Part 2 needs: `recharts`, `react-router-dom`. Nothing else yet — add libraries when a phase actually requires one, not speculatively.

### 0.4 — Decide the dev proxy

The gateway runs on `:8080` (public) and `:9090` (admin). Vite's dev server runs on `:5173`. Configure Vite's proxy so `/v1/*` and `/admin/*` reach the right port in development without CORS headaches. Record the choice — it affects how the production build gets served in Phase 10.

### 0.5 — Read `DESIGN.md`

Before writing any component. It is the authority on everything visual; this plan will not repeat it.

### ✅ Phase 0 checklist

- [ ] Postgres container up, healthy, connectable
- [ ] Node 20+ available in the environment you'll actually develop in
- [ ] `web/` scaffolded, `npm run dev` serves a blank Vite app
- [ ] Vite dev proxy configured for both gateway ports
- [ ] `DESIGN.md` read

---

## Phase 1 — Request-Log Persistence

The one backend prerequisite. Request Logs and cost trends both need one durable row per request; Redis holds current state and Prometheus holds aggregates, neither is a queryable history.

### Step 1.1 — Schema and migrations

Design the `requests` table. At minimum: request ID (primary key, the one already generated in Part 1), timestamp, team ID, requested model, served model, provider, status code, input tokens, output tokens, cost in integer micro-dollars, latency ms, gateway overhead ms, whether a fallback occurred, whether it was a cache hit (nullable until Phase 7), quality score (nullable until Phase 9), and the trace ID.

Index deliberately, not reflexively — you will filter by team, by time range, by status, and by provider. Write the indexes those queries need and no others; record why in `DECISIONS.md`.

Use a migration file, not schema-on-boot. `migrations/0001_requests.sql` applied by a migration step, so the schema is versioned and reviewable.

**Never store prompt or response content.** Same rule as tracing in Part 1 — the log is metadata about requests, never their contents. This is a hard boundary, not a default.

### Step 1.2 — The writer

Write the row *after* the response has been delivered, never before. This must not add latency to the request path and must not fail the request if Postgres is unreachable.

Design decision to make and record 🧠: buffered channel with a background flusher, or write-per-request in a goroutine? Consider what happens under the 60 req/s sustained load from Part 1's k6 run, and what happens to buffered rows on graceful shutdown (Part 1 drains in-flight requests — do buffered log rows get flushed too?).

Postgres down → log the failure, increment a metric, drop the row, serve the request normally. **Fail open, always.**

### Step 1.3 — The query API

Admin port. `GET /admin/requests` with filters (team, status, provider, model, cache status, time range), cursor-based pagination, and a sensible default page size.

Team-scoped by default: a non-admin team's key returns only its own rows, enforced server-side in the handler, not by a query parameter the client could tamper with. Admin keys may pass a `team` filter to see across teams.

`GET /admin/requests/{id}` returns one row with everything, including the trace ID so the UI can deep-link to Jaeger.

### Step 1.4 — Retention

An unbounded table is a slow leak. Add a configurable retention window (default 30 days) and a periodic cleanup. Decide 🧠: delete rows, or aggregate old rows into a daily summary table before deleting? The second preserves long-range cost trends without keeping every row — but it is more work. Either is defensible; pick one and write down why.

### ✅ Phase 1 checklist

- [ ] Migration applies cleanly to an empty database, and is idempotent on re-run
- [ ] A successful request writes exactly one row with correct values in every column
- [ ] A 429 and a 402 also write rows — failures are logged, not just successes
- [ ] A fallback request records both requested and served model, and the fallback flag
- [ ] Cost in the row matches the cost header on the response exactly
- [ ] Stop Postgres, send traffic: every request still succeeds, failures are logged and counted, no request errors
- [ ] Restart Postgres: writing resumes without a gateway restart
- [ ] Graceful shutdown does not silently lose buffered rows (or the loss is documented as accepted)
- [ ] `GET /admin/requests` paginates correctly past 1,000 rows, and filters return correct subsets
- [ ] A non-admin key cannot retrieve another team's rows, even with an explicit `team` parameter
- [ ] No prompt or response text appears anywhere in the table
- [ ] `go test -race ./...` clean
- [ ] `DECISIONS.md`: write path design, index choices, retention strategy

---

## Phase 2 — Frontend Shell + Overview

First visible output. Refer to `DESIGN.md` throughout — this phase specifies data and behaviour only.

### Step 2.1 — Session and auth

The frontend needs to know who it is. Simplest workable model: the user pastes a team API key, it's held in memory (and optionally `sessionStorage`), and every request sends it as a bearer token. No login backend, no user accounts — Part 1's teams *are* the identity model.

Add `is_admin: true` to one team in `configs/teams.yaml`. Add `GET /admin/me` returning the calling team's identity, admin flag, limits, and current spend — the frontend calls this once on load to decide what to render.

**Enforce admin server-side.** Every admin-only endpoint returns 403 for a non-admin key regardless of what the UI shows. The frontend gate is courtesy; the API gate is the actual control. Verify this with a direct `curl`, not just by clicking around.

### Step 2.2 — App shell and routing

Sidebar navigation, five routes, one page component each. Non-admin users don't see admin-only routes, and hitting one directly redirects rather than rendering a broken screen.

Handle three global states properly from the start: loading, error, and empty. They will be needed on every screen, and retrofitting them is worse than building them once now.

### Step 2.3 — The metrics summary endpoint

The frontend does not query Prometheus. Add `GET /admin/summary` on the gateway that queries Prometheus server-side and returns exactly what Overview needs, shaped for the UI: request counts, overhead percentiles, error rate, cache hit rate, cost, and per-provider health.

Time-range parameter (`1h`, `24h`, `7d`). Cache the response briefly (a few seconds) so a 3-second poll from several open tabs doesn't hammer Prometheus.

Prometheus unreachable → return what's available with an explicit `degraded: true` flag rather than a 500. The dashboard showing partial data beats the dashboard showing an error page.

### Step 2.4 — Overview

Data it shows:

- **KPI row** — requests in range, overhead p95, error rate, cache hit rate (empty-stated until Phase 7), cost in range
- **Traffic and latency charts** — request volume over time, overhead percentiles over time. Recharts, styled per `DESIGN.md`
- **Provider health strip** — per provider: status, latency, error rate. Live
- **Circuit breaker state** — per provider+model: closed / half-open / open
- **Live request feed** — the most recent N rows from Phase 1's log, updating without a manual refresh

Scoping: non-admin sees only their own team's numbers; admin sees all teams with a team filter.

### Step 2.5 — Making it live 🧠

Decide and record: SSE from the gateway, or polling? Polling is simpler and adequate at this scale; SSE is more impressive and you already have streaming infrastructure. Either is fine — but the KPI numbers and the request feed must visibly update while someone is watching, without a refresh. A dashboard that requires reloading does not read as a live system.

Whatever you pick: pause updates when the tab is hidden, and back off on repeated failures rather than retrying at full rate against a gateway that's down.

### Step 2.6 — Grafana link

One "Open in Grafana ↗" affordance on Overview, opening the real running Grafana in a new tab. The point is that your dashboard and Grafana read the same Prometheus — your charts are styled to match the product, not a replacement for the real observability stack.

### ✅ Phase 2 checklist

- [ ] Pasting a valid team key loads the app; an invalid key shows a clear error, not a blank screen
- [ ] `GET /admin/me` returns correct identity and admin flag
- [ ] A non-admin key hitting an admin endpoint directly via `curl` gets 403 — verified outside the UI
- [ ] All five routes render; admin-only routes are unreachable for non-admins
- [ ] Loading, error, and empty states exist on Overview and are visibly correct
- [ ] Every KPI matches what Prometheus reports for the same window — cross-checked manually
- [ ] Provider health and breaker state match `/admin/providers/health`
- [ ] Numbers and the request feed update live, with no manual refresh
- [ ] Stop Prometheus: Overview degrades to partial data with a visible indicator, and does not error
- [ ] Stop the gateway: the UI shows a clear disconnected state and backs off instead of hammering
- [ ] Cache-hit-rate widget renders an empty state, not a zero or a fake number
- [ ] Non-admin sees only their own team's data
- [ ] `DECISIONS.md`: live-update mechanism, summary endpoint caching, Grafana-link rationale

---

## Phase 3 — Playground

The single most persuasive screen. Someone types a prompt and watches the entire system respond.

### Step 3.1 — Streaming request/response

A prompt input, model selector (only models the team is allowed — from `/admin/me`), and streaming toggle. On send, call the real `POST /v1/chat/completions` with `stream: true` and render tokens as they arrive.

This works because Part 1's SSE passthrough already flushes per chunk. If the response appears all at once instead of progressively, that is a real bug in the streaming path surfacing — investigate it rather than working around it in the UI.

### Step 3.2 — The metadata panel

Below the response, from the response headers Part 1 already sets: provider, requested model, served model, gateway overhead, latency, tokens in/out, cost, rate-limit remaining, and — when it happened — a clear fallback indicator naming both models.

Cache hit/miss placeholder here too, empty until Phase 7.

### Step 3.3 — Error states as first-class output

429, 402, and 503 are not failures of the screen — they are the system working. Render each distinctly and legibly: the rate-limit response with its `Retry-After`, the budget response with the team's cap and current spend, the 503 with its per-candidate breakdown of what was tried and why each failed.

This is a demo asset. Being able to trigger a 402 live and show a clean, informative response is worth more than another happy-path feature.

### Step 3.4 — Request history

Recent prompts from this session, click to re-run. Session-scoped, in memory — this is convenience, not persistence, and it must not duplicate Request Logs.

### ✅ Phase 3 checklist

- [ ] A streaming request renders progressively, visibly token by token
- [ ] A non-streaming request renders correctly through the same UI
- [ ] Every metadata field matches the response headers exactly
- [ ] The model selector offers only models the team's allowlist permits
- [ ] Trigger a 429 by sending fast: rendered clearly with the retry hint
- [ ] Trigger a 402 by exhausting a small budget: rendered with cap and spend
- [ ] Kill all providers: 503 renders with the per-candidate breakdown intact
- [ ] Kill the primary only: response succeeds and the fallback indicator names both models
- [ ] Navigating away mid-stream cancels the request — verify upstream usage stops
- [ ] Cache indicator shows an empty state, not a fabricated value
- [ ] `DECISIONS.md`: any deviation from the raw API contract, and why

---

## Phase 4 — Live Ops

Where the resilience work from Part 1 becomes visible. This screen is the demo.

### Step 4.1 — Provider panel

Per provider: current status, latency, error rate, last check time, and recent status transitions with their reasons — the history `/admin/providers/health` already returns. Admin-only.

### Step 4.2 — Circuit breaker visualisation

Per provider+model: the three-state machine, rendered live. Current state, failure count against threshold, cooldown remaining when open, and probe status when half-open.

The manual breaker reset from Part 1 gets a control here.

### Step 4.3 — Failure simulation

The chaos endpoint from Phase 7 of Part 1, with a UI. Per provider: force errors, add latency, force rate-limit responses, simulate dropped connections. A clearly visible indicator when chaos is active anywhere, and a single control to clear all of it.

The endpoint already refuses unless `SWITCHYARD_ENV=dev` and the chaos flag are both set — verify the UI degrades gracefully when chaos is unavailable rather than showing controls that silently do nothing.

**The chain reaction is the point.** Clicking "fail groq" must visibly produce, in sequence and within seconds, on this screen: health flipping healthy → degraded → down, the breaker flipping closed → open, and subsequent requests in the feed showing fallback. If that sequence isn't legible in real time, this screen has not achieved its purpose.

### Step 4.4 — Load simulator

Generate concurrent traffic from the browser against the gateway. Configurable concurrency and duration. Live readout while it runs: RPS, successes, 429s, p50/p95/p99, and the breakdown by status.

This is a demo instrument, not a replacement for k6 — the authoritative numbers still come from the committed load-test script in Phase 10. Say so in the UI so nobody mistakes browser-generated numbers for benchmark results.

Admin-only. Bounded — a hard ceiling on concurrency and duration so it can't be used to accidentally exhaust a real provider quota.

### ✅ Phase 4 checklist

- [ ] Provider panel matches `/admin/providers/health` including transition history
- [ ] Breaker visual matches actual breaker state for every provider+model
- [ ] Manual breaker reset works from the UI and is reflected immediately
- [ ] Chaos controls are admin-only and absent for non-admins
- [ ] With chaos disabled by env, the UI states why rather than showing dead controls
- [ ] **The full chain reaction is visible live**: fail a provider → health degrades → breaker opens → next requests show fallback
- [ ] Recovery is equally visible: clear chaos → half-open probe → breaker closes → traffic returns
- [ ] Load simulator reports RPS, status breakdown, and percentiles live while running
- [ ] Load simulator respects its concurrency and duration ceilings
- [ ] UI states plainly that simulator numbers are indicative, not benchmark results
- [ ] `DECISIONS.md`: simulator bounds and why, chaos gating in the UI

**Tag: `part2-milestone-1`** — the gateway is now demonstrable end to end through a UI.

---

## Phase 5 — Request Logs

### Step 5.1 — The table

Backed by Phase 1's query API. Columns: time, request ID, team (admin only), provider, requested/served model, status, latency, overhead, tokens, cost, cache status, fallback indicator.

Cursor pagination — not offset. At 30 days of retention this table gets large, and offset pagination degrades.

### Step 5.2 — Filters

Team (admin only), status, provider, model, cache status, fallback-only, and time range. Filters reflected in the URL so a filtered view is shareable and survives a refresh.

Server-side filtering, always. Never fetch a large page and filter in the browser.

### Step 5.3 — Row detail

Expanded view of one request: every stored field, the routing decision, the fallback chain if one occurred, and a deep link to the trace in Jaeger by trace ID.

The Jaeger link is a meaningful integration point — from a row in your product to the full distributed trace of that exact request is a genuinely strong thing to demonstrate.

### ✅ Phase 5 checklist

- [ ] Table loads and paginates smoothly past 10,000 rows
- [ ] Every filter returns correct results, verified against a direct API call
- [ ] Filters persist across refresh via the URL
- [ ] Filtering happens server-side — confirmed in the network tab
- [ ] Row detail shows every stored field accurately
- [ ] The Jaeger deep link opens the correct trace for that exact request
- [ ] A non-admin sees only their own rows and has no team filter
- [ ] Empty state is clear when filters match nothing
- [ ] `DECISIONS.md`: pagination strategy, filter design

---

## Phase 6 — Usage & Cost

Where Phase 4 and Phase 6 of Part 1 pay off visually.

### Step 6.1 — Team spend

Per team: current month spend against budget, percentage consumed, warning state at 80%, blocked state at 100%. Non-admin sees only their own.

Source of truth is Redis (Part 1's budget counters), not a sum over the request log. Those two should agree — and if they don't, that is a real bug worth finding. Add a reconciliation check that compares them and surfaces a discrepancy rather than hiding it.

### Step 6.2 — Cost trends

From the request log: cost over time, by provider, by model, by team (admin). Recharts per `DESIGN.md`.

### Step 6.3 — Cost attribution

Two panels that are currently placeholders and become real later:

- **Cost saved by cache** — empty until Phase 7
- **Cost shifted by fallback** — real now. Part 1 already logs the cost delta on every fallback; surface it. This is the answer to "what did resilience cost you," and having a number for it is unusual and worth showing

### Step 6.4 — Team management (admin)

A UI over Part 1's existing admin API: view teams, adjust rate limits and budgets, reset a budget, view API key metadata (never the key itself, and never the hash).

Every change goes through the existing audit-logged endpoints. No new mutation paths.

### ✅ Phase 6 checklist

- [ ] Spend per team matches Redis exactly
- [ ] Redis spend and request-log sum agree; a discrepancy is surfaced, not hidden
- [ ] 80% warning and 100% blocked states render correctly — verified by actually crossing both
- [ ] Cost trends match a manual aggregation over the same window
- [ ] Fallback cost delta is real and non-zero after inducing a fallback
- [ ] Cache savings panel shows an empty state
- [ ] Editing a limit or budget through the UI takes effect immediately, no restart
- [ ] Budget reset works and is reflected everywhere
- [ ] No API key or hash is ever sent to the browser
- [ ] Non-admin cannot see or edit other teams
- [ ] `DECISIONS.md`: reconciliation approach, attribution methodology

---

## Phase 7 — Semantic Cache 🧠

The first Part 2 backend feature. It sits on the hot path — treat overhead as a hard constraint.

### Step 7.1 — Embeddings

Choose an embedding source and record why 🧠. Options: a hosted embedding API (simple, costs money per lookup, adds network latency to the hot path) or a local model via Ollama (free, no external dependency, but adds local compute).

The trade-off is real: an embedding call that takes 80ms destroys your overhead budget. Measure before committing. If the hot-path cost is too high, that itself is the finding — document it.

### Step 7.2 — Cache key design

Semantically similar prompts should hit; genuinely different requests must not. The key must incorporate everything that changes the meaning of a response: system prompt, model, temperature, max tokens. Two identical user prompts under different system prompts are different requests and must not share an entry.

Store in Redis: the embedding, the response, token counts, model, timestamp, TTL, and hit count.

### Step 7.3 — Lookup and threshold

On each request: embed, nearest-neighbour search, compare similarity to a configurable threshold. Above → serve from cache. Below → proceed normally, then store.

Threshold is the central tension of this feature: too low and you serve subtly wrong answers, too high and hit rate collapses. Make it configurable, and expose a tuning endpoint that replays historical requests at different thresholds so the trade-off is measurable rather than guessed. That measurement is the interview material here.
  
### Step 7.4 — TTL and invalidation

Not all responses age equally. Time-sensitive prompts need short TTLs or no caching at all; stable factual ones can live much longer. Decide how TTL is assigned and record it.

Invalidation triggers: system prompt change, model change, manual purge by team or key prefix.

### Step 7.5 — Streaming

A cache hit for a streaming request should still stream — returning it as one lump changes the client's experience based on an implementation detail. Replay stored chunks at a plausible cadence, or return instantly and document the deviation. Either is defensible; decide deliberately.

### Step 7.6 — Wire the placeholders

Overview's cache hit rate, Playground's cache indicator, Request Logs' cache column, and Usage & Cost's savings panel all go from empty state to real data. **No UI restructuring** — if any of these needs more than a data change, the placeholder was designed wrong and that's worth noting.

Add cache metrics to Prometheus: hit rate, lookup latency, similarity distribution, near-misses just below threshold.

### ✅ Phase 7 checklist

- [ ] Identical prompt twice → second is a cache hit
- [ ] Semantically similar phrasing → hit, at the configured threshold
- [ ] Genuinely different prompts → miss
- [ ] Same user prompt, different system prompt → **miss** (verified explicitly)
- [ ] Same prompt, different model or temperature → miss
- [ ] **Overhead p95 still under 10ms with cache lookup on the hot path** — measured, not assumed
- [ ] Cache hits are measurably faster than misses end to end
- [ ] Redis down → every request still succeeds, cache silently disabled, metric fired
- [ ] TTL expiry works; expired entries miss and re-populate
- [ ] Invalidation by system-prompt change and by manual purge both work
- [ ] Streaming cache hits behave per the documented decision
- [ ] All four UI placeholders now show real data with no layout change
- [ ] Cache cost savings are computed from real token counts, not estimated
- [ ] `go test -race ./...` clean
- [ ] `DECISIONS.md`: embedding source, key design, threshold methodology, TTL strategy, streaming decision

---

## Phase 8 — Cost-Aware Routing 🧠

Reuses Part 1's tier and fallback machinery — same YAML tiers, a different reason for choosing one.

### Step 8.1 — Complexity classification

Classify each prompt into tiers. Start with cheap heuristic features — token count, presence of reasoning verbs, number of constraints, whether context was supplied, requested output format — before reaching for a model. A classifier that itself calls an LLM defeats the purpose on both cost and latency.

Build a labelled set by hand. Accuracy above ~80% is sufficient for V1; you are building the routing skeleton, not a research artifact.

### Step 8.2 — Routing policy

Map tier → the cheapest model capable of handling it, using the tiers already defined in `providers.yaml`.

Three constraints that are not negotiable:

- **Allowlists still win.** Never route a team to a model it isn't permitted to use — same rule as fallback in Part 1.
- **Explicit beats inferred.** If the caller named a specific model, honour it. Cost-aware routing applies when the caller expressed a preference for a *tier*, or opted in. Silently downgrading a request someone explicitly made is a correctness violation, not an optimisation.
- **Health still gates.** A cheap model whose breaker is open is not a candidate.

### Step 8.3 — Transparency

Response headers state which model was chosen and why. Playground and Request Logs surface it. A user must always be able to see that routing happened and what it decided — a cost optimiser that hides its decisions is not defensible.

### Step 8.4 — Savings measurement

Compute what the request *would* have cost at the top tier versus what it did cost. Aggregate into the Usage & Cost screen. This is the headline number for this feature and it must come from real logged data, not a projection.

### ✅ Phase 8 checklist

- [ ] Classifier accuracy measured on a held-out set and recorded
- [ ] Classification latency is negligible on the hot path — measured
- [ ] Simple prompts route to the cheap tier; complex prompts route up
- [ ] An explicitly named model is never silently overridden
- [ ] Routing never violates a team's allowlist — verified with a restricted team
- [ ] An open breaker removes a model from candidacy
- [ ] Routing decision and rationale appear in headers, Playground, and Request Logs
- [ ] Savings are computed from real logged costs
- [ ] **Overhead p95 still under 10ms with both cache and routing on the hot path**
- [ ] Classifier failure → falls back to the requested model, request still succeeds
- [ ] `DECISIONS.md`: feature choice, why heuristics before a model, explicit-beats-inferred rationale

---

## Phase 9 — Async Quality Verification

The feature that keeps Phases 7 and 8 honest. Both trade correctness risk for cost savings; this measures whether that trade is holding.

### Step 9.1 — Sampling

Not every response — that would be expensive and slow. Sample deliberately:

- A configurable percentage of routed responses
- Every cache hit whose similarity was close to the threshold
- Every response served by a downgraded tier
- Optionally, anything a user flagged

Record the sampling policy; it determines what your quality numbers actually mean.

### Step 9.2 — The worker

A background worker consuming a queue. **Never on the request path** — this is the strictest version of the telemetry rule. It must add exactly zero latency and its failure must be invisible to callers.

Scoring: LLM-as-judge on a small scale with a written rubric. Store the score against the request row from Phase 1.

### Step 9.3 — Feedback

Two loops:

- **Cache** — low scores on near-threshold hits are evidence the threshold is too permissive. Surface this; a manual threshold adjustment based on real data is a stronger story than automatic tuning you can't explain.
- **Routing** — low scores on downgraded requests become training examples for the classifier.

Neither loop should act automatically without a written rationale. Surfacing the signal and adjusting deliberately is more defensible than a black box that retunes itself.

### Step 9.4 — Surfacing

Quality score on Request Logs rows and in Playground. Quality trend on Overview. Provider and model comparison, and low-quality alerting, on Usage & Cost or a dedicated panel — placement per `DESIGN.md`.

### ✅ Phase 9 checklist

- [ ] Sampling follows the documented policy; the sampled proportion matches configuration
- [ ] **Zero measurable latency added to the request path** — verified against Phase 8's baseline
- [ ] Worker failure or backlog leaves request handling completely unaffected
- [ ] Scores are stored against the correct request rows
- [ ] Near-threshold cache hits are sampled at a higher rate than baseline
- [ ] Downgraded-tier responses are always sampled
- [ ] Quality data appears correctly in Request Logs, Playground, and Overview
- [ ] A deliberately bad response scores low — verified by planting one
- [ ] Judge model unavailable → sampling degrades silently, no request impact
- [ ] `DECISIONS.md`: sampling policy, rubric design, why feedback is surfaced rather than automatic

**Tag: `part2-milestone-2`** — cache, routing, and verification are complete and measured.

---

## Phase 10 — Final Load Test, Demo & Wrap

### Step 10.1 — Rerun the load test

Same k6 script as Part 1, extended with cache-hit-inducing repeated prompts and a mix that exercises routing.

Report, with Part 1's numbers alongside for comparison:

- Overhead p50/p95/p99 with cache and routing active
- Cache hit rate at steady state and how quickly it converged
- Cost saved by cache and by routing, separately
- Quality scores across sampled responses
- Whether rate limiting and failover still behave exactly as they did in Part 1

**Fix the two known issues from Part 1's results before this run**: the `p(95)<10` threshold reporting `false` while the actual value passed, and the missing caption explaining that `http_req_failed` is inflated by intentional 429/402/503 responses.

Run on Linux, not Windows, for the overhead numbers — Part 1 documented the clock-resolution caveat and this is where it gets resolved.

### Step 10.2 — Production build and serving

Build the frontend and decide how it's served 🧠: the gateway serving static files from an embedded filesystem (one binary, one container, simplest deployment) or a separate nginx container (conventional, more moving parts). Either works — pick one and record why.

`docker compose up` must still bring up the complete system including the UI, with no manual steps beyond the documented Ollama caveat.

### Step 10.3 — Demo script

Extend Part 1's `demo.sh` into a full narrative that now includes the UI:

1. Overview loading with live traffic
2. A Playground request streaming, with metadata
3. The same prompt again — cache hit, visibly instant
4. A complex prompt routing up, a simple one routing down
5. Failure simulation: health degrades, breaker opens, fallback engages — all visible live
6. Recovery: probe succeeds, breaker closes
7. Rate limit hit under the load simulator
8. Budget exhausted → 402
9. Request Logs filtered to the failures just produced, with a Jaeger deep link
10. Usage & Cost showing real savings from cache, routing, and the cost of fallback

Each step pauses for narration.

### Step 10.4 — Documentation

- README updated: Part 2 features, new numbers, screenshots of every screen
- `DECISIONS.md` reviewed end to end — every Part 2 phase has an entry
- A short case study framing the whole project: the problem, the architecture, the numbers, and one design decision you'd defend hardest
- A Loom walkthrough following the demo script

### ✅ Phase 10 checklist

- [ ] Load test rerun on Linux, results committed with the reproduction command
- [ ] Both Part 1 k6 reporting issues fixed
- [ ] **Overhead p95 under 10ms with cache, routing, and logging all active**
- [ ] Cache hit rate and cost savings measured, not estimated
- [ ] Rate limiting and failover verified unchanged from Part 1
- [ ] Goroutine count and memory return to baseline after the run
- [ ] `docker compose up` from a fresh clone brings up the complete system including UI
- [ ] `demo.sh` runs the full narrative end to end without manual intervention
- [ ] README leads with numbers; every one is reproducible
- [ ] `DECISIONS.md` covers every phase of both parts
- [ ] Loom recorded
- [ ] `go test -race ./...` clean; integration suite passes
- [ ] Coverage above 80% on `internal/cache`, `internal/router`, `internal/logstore`

**Tag: `part2-complete`**

---

## Exit interview

Answer out loud, from memory, no notes. Any hesitation is a gap to close now.

1. Why Postgres for request logs when you already had Redis, Prometheus, and Jaeger?
2. Your log writer is asynchronous. What happens to buffered rows on graceful shutdown, and why is that acceptable?
3. Your semantic cache serves a response to a prompt that isn't identical. How do you know it's the right answer?
4. How did you choose the similarity threshold, and what does the trade-off curve actually look like?
5. Two identical user prompts, different system prompts. Why must they not share a cache entry, and how do you enforce it?
6. Cache lookup is on the hot path. What did it cost you in overhead, and how do you know?
7. Why does your complexity classifier use heuristics rather than an LLM?
8. A caller explicitly asks for your most expensive model on a trivial prompt. What do you do, and why?
9. Your quality worker is async. Prove it adds zero latency.
10. Your sampling policy oversamples near-threshold cache hits. Why those specifically?
11. Cache and routing both trade correctness risk for cost. How would you know if that trade stopped being worth it?
12. Redis spend and the request-log sum disagree. Which do you trust, and how do you find out why?
13. Why did you build your own charts rather than embedding Grafana panels?
14. A non-admin user opens devtools and calls an admin endpoint. What stops them?
15. Which single decision across both parts would you defend hardest, and which would you revisit?
