# SwitchYard — Part 1 Implementation Plan

**An LLM API gateway with rate limiting, budget enforcement, multi-provider failover, and full observability.**

Part 1 covers Phases 0–11 (~4 weeks). At the end of Part 1 you have a working, observable, resilient gateway. Part 2 adds the semantic cache, cost-aware routing, async quality verification, and the React UI.

---

## Ground Rules

Read this section before writing any code. It applies to every phase.

### Language & stack (Part 1)

| Component | Choice | Notes |
|---|---|---|
| Language | Go 1.22+ | stdlib `net/http` + `chi` router only — no heavyweight framework |
| State | Redis 7 | rate limit buckets, budget counters, health state |
| Config | YAML + hot reload | providers and teams; no DB in Part 1 |
| Tracing | OpenTelemetry (Go SDK) | OTLP exporter → Jaeger (local) |
| Metrics | Prometheus | `promhttp` handler on a separate admin port |
| Dashboards | Grafana | provisioned via files, not clicked-together |
| Providers | OpenAI, Anthropic, Ollama | Ollama runs locally, free, no key needed |
| Deployment | Docker Compose | one command spins up everything |

**Postgres is deliberately deferred to Part 2.** Request-log persistence belongs with the UI work. Adding it now is scope you can't demo yet.

### Working discipline

- **One phase per Claude Code session.** `/clear` between phases. Context compounding is what burns Pro quota, not message count.
- **Use `/model opusplan`.** Override to Opus explicitly for the phases marked 🧠 below.
- **Write these by hand yourself:** the token bucket (Phase 3) and the circuit breaker state machine (Phase 7). They are small, and they are the two things an interviewer will grill you on. Slow and yours beats fast and generated.
- **Read every diff before accepting.** You are learning Go on this project. If you don't read the code, you'll have a Go project on your resume and no Go in your hands.
- **Append to `DECISIONS.md` after every phase.** Format: what you chose, what you rejected, why. This file is your interview prep doc — same role the Master MDs played for Syntropy and ThreadWright.
- **Commit at every checklist completion.** Tag phase completions: `git tag phase-3-complete`.

### Non-negotiable design constraints

These are the things that make this project interview-defensible. Do not let them get optimized away:

1. **The gateway must never be the reason a request fails.** Every dependency (Redis, telemetry, health checker) has a defined failure mode. Decide fail-open vs fail-closed for each, and document it.
2. **Gateway overhead target: p95 under 10ms** excluding provider time. Measure it from Phase 1 onward, not at the end.
3. **Streaming must stay streaming.** No buffering the full response before forwarding. Chunks go out as they arrive.
4. **Every number you eventually put on your resume must come from a committed script a reviewer can rerun.** No hand-typed figures.

---

## Phase 0 — Prerequisites & Setup

**You do this manually. No Claude Code.**

### Step 0.1 — Install toolchain

- [ ] Go 1.22+ (`go version`)
- [ ] Docker + Docker Compose v2 (`docker compose version`)
- [ ] Redis CLI (optional, for poking at state: `redis-cli`)
- [ ] `k6` or `vegeta` for load testing later (pick one; k6 is friendlier)
- [ ] Ollama installed locally, with one small model pulled (`ollama pull llama3.2:3b`)

### Step 0.2 — Get API keys

- [ ] OpenAI API key with a small hard spend cap set in their dashboard
- [ ] Anthropic API key with a small hard spend cap set
- [ ] Store both in a `.env` file at repo root
- [ ] `.env` is in `.gitignore` **before** the first commit

### Step 0.3 — Repo skeleton

```
switchyard/
├── cmd/
│   └── gateway/
│       └── main.go
├── internal/
│   ├── config/
│   ├── provider/
│   ├── proxy/
│   ├── auth/
│   ├── ratelimit/
│   ├── budget/
│   ├── health/
│   ├── resilience/
│   ├── telemetry/
│   └── admin/
├── configs/
│   ├── providers.yaml
│   └── teams.yaml
├── deploy/
│   ├── docker-compose.yml
│   ├── prometheus/
│   └── grafana/
│       ├── dashboards/
│       └── provisioning/
├── scripts/
├── test/
├── .env.example
├── .gitignore
├── CLAUDE.md
├── DECISIONS.md
├── PART1_PLAN.md
├── go.mod
└── README.md
```

- [ ] Create the directory tree above
- [ ] `go mod init github.com/<your-handle>/switchyard`
- [ ] `git init`, initial commit
- [ ] Create the GitHub repo, push

### Step 0.4 — Go fundamentals warm-up

Before Phase 1, make sure you're comfortable with these. Spend a day if you need to — it will save you three later.

- [ ] `context.Context`: cancellation, deadlines, passing values
- [ ] Goroutines, channels, `select`
- [ ] `sync.Mutex`, `sync.RWMutex`, `atomic`
- [ ] Interfaces and how Go does implicit satisfaction
- [ ] Error wrapping: `fmt.Errorf("...: %w", err)`, `errors.Is`, `errors.As`
- [ ] `net/http` middleware pattern (`func(http.Handler) http.Handler`)
- [ ] `io.Reader` / `io.Writer` and `http.Flusher` (you need this for streaming)

### ✅ Phase 0 Checklist

- [ ] `go version` shows 1.22+
- [ ] `docker compose version` works
- [ ] `ollama run llama3.2:3b "hello"` returns a response
- [ ] OpenAI and Anthropic keys tested with a raw `curl`, both return 200
- [ ] Spend caps set on both provider dashboards
- [ ] `.env` gitignored, `.env.example` committed with dummy values
- [ ] Repo pushed to GitHub, directory tree in place
- [ ] `CLAUDE.md` in repo root (generated separately)
- [ ] You can explain what `http.Flusher` does without looking it up

---

# WEEK 1 — Core Proxy

---

## Phase 1 — Provider Abstraction & Core Proxy 🧠

**Use Opus for the interface design in Step 1.1.** Everything downstream depends on it; a bad abstraction here costs you rework in Phases 6, 7, and all of Part 2.

### Step 1.1 — Design the canonical request/response types

Define the internal format that every provider translates to and from. The caller speaks SwitchYard's format; SwitchYard speaks each provider's dialect.

- [ ] `provider.Request`: model, messages, temperature, max tokens, stream flag, stop sequences
- [ ] `provider.Response`: content, finish reason, `Usage{InputTokens, OutputTokens}`, model actually served, provider name, latency
- [ ] `provider.Error`: a typed error carrying an HTTP status, a provider name, and — critically — a `Retryable bool` and a `Kind` enum (`RateLimited`, `Timeout`, `AuthFailed`, `ContentPolicy`, `ServerError`, `NetworkError`)

The `Retryable` / `Kind` classification is load-bearing. Phase 6 and Phase 7 both depend on it. Get it right now.

- [ ] Decide: does SwitchYard mirror the OpenAI wire format for its public API, or define its own? Mirroring means clients can switch by changing a base URL — a strong demo. Document the choice in `DECISIONS.md`.

### Step 1.2 — Define the Provider interface

```go
type Provider interface {
    Name() string
    Complete(ctx context.Context, req Request) (*Response, error)
    Stream(ctx context.Context, req Request) (StreamReader, error)
    SupportsModel(model string) bool
    Ping(ctx context.Context) error   // used by Phase 5 health checks
}
```

- [ ] Keep the interface this small. Resist adding methods. Anything provider-specific belongs behind it.
- [ ] Write it with `Ping` included from day one so Phase 5 doesn't require touching every provider file again.

### Step 1.3 — Implement the three providers

- [ ] OpenAI adapter: translate to/from `/v1/chat/completions`
- [ ] Anthropic adapter: translate to/from `/v1/messages` (note: system prompt is a top-level field, not a message — this is the classic gotcha)
- [ ] Ollama adapter: `/api/chat`, no auth
- [ ] Each maps provider-specific error responses onto `provider.Error` with correct `Kind` and `Retryable`
- [ ] Each has a per-request timeout from config, not hardcoded

### Step 1.4 — Provider registry + config loading

- [ ] `configs/providers.yaml`: provider name, base URL, env var for key, timeout, models offered, cost per 1M input tokens, cost per 1M output tokens
- [ ] Registry loads YAML at boot, validates it (fail fast on a malformed config — do not start with a broken registry)
- [ ] `registry.ForModel(model string) (Provider, error)` resolves a model name to a provider
- [ ] Real pricing filled in for every model listed. You'll need this in Phase 4.

### Step 1.5 — Minimal HTTP server

- [ ] `chi` router on the main port (8080)
- [ ] Separate admin/metrics listener on 9090 (never expose metrics on the public port)
- [ ] `POST /v1/chat/completions` — non-streaming only for now
- [ ] `GET /healthz` — liveness, always 200 if the process is up
- [ ] `GET /readyz` — readiness, 503 if config didn't load
- [ ] Graceful shutdown on SIGTERM with a drain timeout
- [ ] Structured logging with `log/slog` — JSON output, request ID on every line

### Step 1.6 — Request ID and basic timing

- [ ] Middleware generates a request ID (or honours an inbound `X-Request-ID`)
- [ ] Response carries `X-Switchyard-Request-Id`, `X-Switchyard-Provider`, `X-Switchyard-Model`, `X-Switchyard-Overhead-Ms`
- [ ] Overhead = total handler time minus provider call time. **Start measuring this now.** It's your headline number.

### ✅ Phase 1 Checklist

- [ ] `curl` to SwitchYard returns a real completion from OpenAI
- [ ] Same `curl`, different model, routes to Anthropic
- [ ] Same `curl`, `llama3.2:3b`, routes to Ollama
- [ ] Killing the OpenAI key produces a `provider.Error` with `Kind=AuthFailed, Retryable=false` — not a generic 500
- [ ] Malformed `providers.yaml` prevents startup with a clear error message
- [ ] `X-Switchyard-Overhead-Ms` present on every response and reads under 10ms
- [ ] SIGTERM drains in-flight requests instead of dropping them
- [ ] Logs are JSON, one line per request, request ID present
- [ ] `DECISIONS.md` updated: wire format choice, error taxonomy, timeout strategy
- [ ] You can explain why `Retryable` is a field on the error rather than inferred later

---

## Phase 2 — Streaming Passthrough

This is the trickiest part of Week 1. Budget more time than you think.

### Step 2.1 — Streaming interface

- [ ] `StreamReader` interface: `Recv() (*Chunk, error)`, returns `io.EOF` on clean completion, `Close() error`
- [ ] `Chunk`: delta content, finish reason, optional usage (providers send this differently — OpenAI at the end if requested, Anthropic in `message_delta`)

### Step 2.2 — Per-provider SSE parsing

- [ ] OpenAI SSE: `data: {...}` lines, terminated by `data: [DONE]`
- [ ] Anthropic SSE: named event types (`content_block_delta`, `message_delta`, `message_stop`)
- [ ] Ollama: newline-delimited JSON, not SSE — handle the difference
- [ ] Each normalizes into the same `Chunk` type

### Step 2.3 — Passthrough handler

- [ ] Set `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`
- [ ] Write each chunk and **`Flush()` immediately** — this is what makes it actually stream
- [ ] Accumulate content and token counts in parallel for logging/metrics, without delaying the write
- [ ] Client disconnect (`ctx.Done()`) cancels the upstream provider request — don't keep paying for a stream nobody's reading

### Step 2.4 — Streaming error semantics

- [ ] Error **before** the first byte: return a normal HTTP error status
- [ ] Error **mid-stream**: headers are already sent, so you cannot change status. Emit an SSE error event and close. Document this in `DECISIONS.md` — it's a genuinely interesting interview point.
- [ ] Decide and document: does a mid-stream failure trigger fallback in Phase 6? (Recommended answer: no — partial output has already reached the client. Retrying would duplicate content.)

### ✅ Phase 2 Checklist

- [ ] `curl -N` shows tokens arriving progressively, not in one dump
- [ ] All three providers stream through the same handler
- [ ] Token counts logged correctly for streaming requests
- [ ] Killing the client mid-stream cancels the upstream call (verify: provider dashboard shows no continued usage)
- [ ] Mid-stream provider failure produces an SSE error event, not a hang
- [ ] Streaming and non-streaming paths share the same middleware chain — no duplicated logic
- [ ] `DECISIONS.md`: mid-stream error handling, why no fallback mid-stream

---

# WEEK 2 — Multi-Tenancy & Enforcement

**End of this week is your first genuinely demo-able state.** If everything after this goes wrong, you still have something to show.

---

## Phase 3 — Auth & Rate Limiting

### Step 3.1 — Team config & authentication

- [ ] `configs/teams.yaml`: team ID, name, hashed API key, allowed providers, allowed models, rate limits (RPM + TPM), monthly budget in USD, priority tier
- [ ] API keys hashed at rest (SHA-256 is fine here; document why you didn't use bcrypt — you're comparing on every request and need it fast)
- [ ] Auth middleware: extract bearer token, look up team, attach `*Team` to request context
- [ ] Reject unknown keys with 401, disallowed model with 403 and a message naming what the team *is* allowed to use

### Step 3.2 — Token bucket ✋ **Write this yourself**

Do not generate this. It's ~60 lines and it's the algorithm you'll be asked to explain on a whiteboard.

- [ ] Redis Lua script implementing token bucket: atomic check-and-consume in one round trip
- [ ] Two independent buckets per team: requests-per-minute and tokens-per-minute
- [ ] Lazy refill (compute tokens accrued since last access from a stored timestamp) — do **not** run a background refill goroutine. Understand why: no timers, no drift, works across replicas.
- [ ] Key schema: `switchyard:rl:{team_id}:{rpm|tpm}`
- [ ] TTL on keys so idle teams don't leak memory

Be ready to answer: why token bucket and not leaky bucket or a fixed/sliding window? (Answer involves burst tolerance.)

### Step 3.3 — TPM enforcement problem 🧠

You don't know the output token count until *after* the request completes. This is a real design problem and a great interview talking point.

- [ ] Reserve estimated tokens up front (input tokens counted + `max_tokens` as the ceiling)
- [ ] Reconcile after the response: return the unused reservation to the bucket
- [ ] Handle the failure case: if the request errors, the reservation must still be returned. Use `defer`.
- [ ] Document the approach and the alternative you rejected in `DECISIONS.md`

### Step 3.4 — Rate limit middleware & responses

- [ ] Middleware runs after auth, before the provider call
- [ ] 429 response includes `Retry-After` (seconds until enough tokens accrue) and `X-RateLimit-Limit` / `X-RateLimit-Remaining` / `X-RateLimit-Reset`
- [ ] **Redis-down behaviour:** decide fail-open or fail-closed and document it. Recommended: fail open with a loud log and a metric, because the gateway must never be the reason a request fails. Be ready to defend this — the opposite answer is also defensible.

### Step 3.5 — Priority tiers

- [ ] Team config carries a priority: `realtime` | `batch`
- [ ] When a team is near its limit, `batch` requests are shed first
- [ ] Simplest defensible implementation: `batch` requests are rejected at 80% bucket depletion, `realtime` at 100%. Don't build a full priority queue — it's Part 2 scope at best.

### ✅ Phase 3 Checklist

- [ ] Request without a key → 401; unknown key → 401
- [ ] Team requesting a model outside its allowlist → 403 with a helpful message
- [ ] Exceeding RPM → 429 with correct `Retry-After`
- [ ] Exceeding TPM → 429, and a large `max_tokens` reserves correctly
- [ ] Failed request returns its token reservation (verify in `redis-cli`)
- [ ] Two concurrent clients on one team share one bucket correctly (run 50 parallel requests, count how many got through — the number should match the limit, not exceed it)
- [ ] Stopping Redis: gateway keeps serving, logs the degradation, metric increments
- [ ] `batch` team is shed before `realtime` under pressure
- [ ] **You can explain the token bucket refill math on paper, without the code in front of you**
- [ ] `DECISIONS.md`: bucket algorithm choice, TPM reservation strategy, fail-open rationale

---

## Phase 4 — Budget Enforcement & Admin API

### Step 4.1 — Cost accounting

- [ ] Cost calculator: `(input_tokens × input_price + output_tokens × output_price) / 1_000_000`
- [ ] Prices come from `providers.yaml`, per model — never hardcoded
- [ ] Cost computed after every request, streaming and non-streaming
- [ ] Rounding: use integer micro-dollars internally. Floats accumulate error over thousands of requests, and an interviewer may well ask about this.

### Step 4.2 — Budget tracking

- [ ] Redis counters per team per period: `switchyard:budget:{team_id}:{YYYY-MM}`
- [ ] `INCRBY` in micro-dollars — atomic, no read-modify-write race
- [ ] Check before the request using the *estimated* max cost; reconcile with actual after
- [ ] 80% threshold → warning header `X-Switchyard-Budget-Warning` + log + metric
- [ ] 100% → block with 402 Payment Required and a message stating the cap and current spend

### Step 4.3 — Admin API

Mount on the admin port (9090), separate from the public API.

- [ ] `GET /admin/teams` — list teams with current rate limit state and spend
- [ ] `GET /admin/teams/{id}` — detail
- [ ] `PATCH /admin/teams/{id}` — adjust limits and budget **without a restart**
- [ ] `POST /admin/teams/{id}/reset-budget` — manual reset
- [ ] `GET /admin/providers` — provider list and config (health comes in Phase 5)
- [ ] Every mutation logged with actor, timestamp, before/after values

### Step 4.4 — Config hot reload

- [ ] `fsnotify` watcher on `configs/`, or a `POST /admin/reload` endpoint (pick one, document why)
- [ ] Reload validates before swapping — a bad config must not take down a running gateway
- [ ] Atomic swap via `atomic.Pointer` or `RWMutex`; in-flight requests continue on the old config
- [ ] Reload event logged and exposed as a metric

### ✅ Phase 4 Checklist

- [ ] Cost per request matches manual calculation against provider pricing for all three providers
- [ ] Team hitting 80% gets the warning header, requests still succeed
- [ ] Team hitting 100% gets 402 with current spend in the message
- [ ] `PATCH` raising a budget immediately unblocks the team — no restart
- [ ] Streaming requests are billed correctly (this is easy to get wrong; test it explicitly)
- [ ] Invalid config reload is rejected, gateway keeps running on the old config
- [ ] Admin port is not reachable from the public port's network interface
- [ ] `DECISIONS.md`: micro-dollar integers, pre-check vs post-reconcile, reload mechanism
- [ ] **Git tag `part1-milestone-1` — this is a demo-able gateway**

---

# WEEK 3 — Resilience

---

## Phase 5 — Health Checking

### Step 5.1 — Active health checks

- [ ] Background goroutine per provider, ticking every 30s (configurable)
- [ ] Calls `Provider.Ping()` — a minimal completion request, cheapest model, `max_tokens: 1`
- [ ] Health checks must not consume team rate limits or budgets — they bypass those middlewares entirely
- [ ] Uses its own short timeout, independent of request timeouts

### Step 5.2 — Passive health signals

Active checks alone are too slow to catch a fast-degrading provider. Real requests are your best signal.

- [ ] Rolling window (last 100 requests or last 60s, whichever is shorter) per provider
- [ ] Track: error rate, p99 latency, timeout rate
- [ ] Ring buffer, not an unbounded slice — you're storing this per provider forever

### Step 5.3 — Status computation

- [ ] Three states: `healthy` | `degraded` | `down`
- [ ] `degraded`: error rate above threshold (e.g. 10%) **or** p99 above 3× the rolling baseline
- [ ] `down`: ping failing consecutively N times **or** error rate above a hard threshold (e.g. 50%)
- [ ] Hysteresis on recovery: require N consecutive healthy signals before returning to `healthy`. Without this the status flaps and your Grafana graph looks like a seismograph.
- [ ] State stored in Redis so it's shared if you run multiple gateway replicas
- [ ] Every state transition logged with the reason that triggered it

### Step 5.4 — Expose health

- [ ] `GET /admin/providers/health` — per provider: status, error rate, p99, last check, last transition
- [ ] Health history retained (last 100 transitions) for post-incident review
- [ ] `/readyz` returns 503 only if **all** providers are down — one bad provider is not an unready gateway

### ✅ Phase 5 Checklist

- [ ] All three providers report `healthy` at rest
- [ ] Stopping Ollama → status moves to `down` within two check intervals
- [ ] Restarting Ollama → status returns to `healthy` only after the hysteresis count
- [ ] Feeding artificial errors (bad key) moves a provider to `degraded` before `down`
- [ ] Health checks visible in logs but not counted against any team's limits or budget
- [ ] Status survives a gateway restart (it's in Redis)
- [ ] `/admin/providers/health` shows the transition history with reasons
- [ ] `DECISIONS.md`: active vs passive signals, thresholds and why, hysteresis rationale

---

## Phase 6 — Retry & Fallback Routing

### Step 6.1 — Retry with exponential backoff

- [ ] Retry only when `provider.Error.Retryable == true`
- [ ] Never retry: auth failures, content policy violations, malformed requests (4xx that aren't 429)
- [ ] Always retry-eligible: 429, 5xx, network errors, timeouts
- [ ] Exponential backoff with **full jitter** (`sleep = rand(0, base × 2^attempt)`). Be ready to explain why jitter matters — synchronized retries from many clients create a thundering herd.
- [ ] Cap at 3 attempts, configurable
- [ ] Total retry time must respect the caller's context deadline — never retry past it
- [ ] Honour `Retry-After` from the provider when present, in preference to your own backoff

### Step 6.2 — Fallback chains 🧠

- [ ] Fallback is defined **per model tier, not per specific model.** Config:
  ```yaml
  tiers:
    frontier:
      - {provider: openai, model: gpt-4o}
      - {provider: anthropic, model: claude-sonnet-4-5}
    fast:
      - {provider: anthropic, model: claude-haiku-4-5}
      - {provider: openai, model: gpt-4o-mini}
      - {provider: ollama, model: llama3.2:3b}
  ```
- [ ] Resolution order: requested model → its tier → next healthy option in the tier
- [ ] Skip any provider currently `down`; deprioritize `degraded`
- [ ] Respect team allowlists — never fall back to a provider a team isn't permitted to use, even if it's the only healthy one. Return an error instead. **This is a compliance point and a strong interview answer.**
- [ ] Response headers state what happened: `X-Switchyard-Fallback: true`, `X-Switchyard-Requested-Model`, `X-Switchyard-Served-Model`

### Step 6.3 — Fallback ordering rules

- [ ] Retry the primary first (per 6.1), then fall back. Not the reverse.
- [ ] Once fallen back, do not retry the primary within the same request
- [ ] If the whole chain is exhausted: return 503 with a body listing what was attempted and why each failed. A useful error beats a generic one.
- [ ] Streaming: fallback only permitted before the first byte is written (per Phase 2 decision)

### Step 6.4 — Cost implications

- [ ] Fallback can change cost — a `fast`-tier fallback to a pricier model must bill correctly
- [ ] Log the cost delta on every fallback event
- [ ] Budget check re-runs against the fallback model's price if it's more expensive

### ✅ Phase 6 Checklist

- [ ] Auth failure is never retried (verify in logs: one attempt only)
- [ ] 429 from a provider triggers backoff and retry
- [ ] Provider `Retry-After` header is honoured over computed backoff
- [ ] Backoff delays are visibly jittered across concurrent requests, not identical
- [ ] Retries never exceed the client's context deadline
- [ ] Killing Ollama mid-test → `fast` tier requests fall back to Haiku, response headers show it
- [ ] A team not allowed to use OpenAI does **not** get an OpenAI fallback — it gets an error
- [ ] Full chain exhaustion returns 503 with a per-provider failure breakdown
- [ ] Fallback to a costlier model bills correctly
- [ ] `DECISIONS.md`: retry-then-fallback order, full jitter, tier-based chains, allowlist-over-availability

---

## Phase 7 — Circuit Breaker ✋ **Write this yourself**

The state machine is ~100 lines. It is the single most likely thing to come up in an interview about this project. Hand-write it.

### Step 7.1 — State machine

- [ ] Three states: `Closed` (normal), `Open` (all requests rejected immediately), `HalfOpen` (probing)
- [ ] `Closed → Open`: failure count exceeds threshold within a rolling window
- [ ] `Open → HalfOpen`: after a cooldown period elapses
- [ ] `HalfOpen → Closed`: N consecutive probe successes
- [ ] `HalfOpen → Open`: any probe failure, with the cooldown reset (consider making the cooldown exponential on repeated failures)
- [ ] All transitions logged with the triggering condition

### Step 7.2 — Half-open concurrency 🧠

The subtle part. In `HalfOpen`, you must allow *some* traffic through to test recovery, but not a flood that re-kills a fragile provider.

- [ ] Allow exactly one in-flight probe at a time (use a semaphore or `atomic.CompareAndSwap`)
- [ ] All other requests during a probe are rejected as if `Open`
- [ ] Decide and document: single probe vs percentage-based. Single probe is safer and simpler to defend; percentage recovers faster. Pick one, know the trade-off.

### Step 7.3 — Distributed state

- [ ] Breaker state in Redis so replicas agree
- [ ] But: local in-memory read cache with a short TTL, because reading Redis on every request adds latency to the hot path
- [ ] Document this trade-off — brief inconsistency across replicas in exchange for latency. This is exactly the kind of trade-off senior interviewers probe.

### Step 7.4 — Integration with fallback

- [ ] `Open` breaker → skip that provider entirely in chain resolution, no attempt, no timeout wait
- [ ] Breaker is per **provider+model**, not per provider. One bad model shouldn't take out a whole provider.
- [ ] Breaker state feeds the health status from Phase 5 (a provider with an open breaker is at least `degraded`)
- [ ] `POST /admin/providers/{name}/breaker/reset` for manual intervention

### Step 7.5 — Failure simulation harness

You need this to demo and to test. Build it properly.

- [ ] `scripts/chaos.go` or a `/admin/chaos` endpoint (admin port only, disabled by default via env flag)
- [ ] Modes: force a provider to error, inject latency, return 429s, drop connections
- [ ] Targetable at a specific provider or model
- [ ] **Guarded so it can never be enabled in a non-dev environment**

### ✅ Phase 7 Checklist

- [ ] Injecting failures opens the breaker at the configured threshold, not before
- [ ] `Open` breaker rejects instantly — measure it, should be sub-millisecond, no provider call attempted
- [ ] After cooldown, exactly one probe goes through in `HalfOpen`, others are rejected
- [ ] Probe success after N consecutive → closes; single probe failure → reopens with reset cooldown
- [ ] Breaker for `gpt-4o` opening does not affect `gpt-4o-mini`
- [ ] Open breaker on the primary → fallback engages with no latency penalty from the dead provider
- [ ] State visible in `/admin/providers/health`; manual reset works
- [ ] Chaos endpoint refuses to enable without the explicit dev env flag
- [ ] **You can draw the state machine on a whiteboard, including the half-open concurrency guard**
- [ ] `DECISIONS.md`: single probe rationale, per-provider+model granularity, Redis-with-local-cache trade-off
- [ ] **Git tag `part1-milestone-2` — the gateway is now resilient**

---

# WEEK 4 — Observability

This week is the project's actual thesis. If you run short on time anywhere in Part 1, cut scope elsewhere — not here.

---

## Phase 8 — OpenTelemetry Tracing

### Step 8.1 — SDK setup

- [ ] OTel Go SDK, OTLP gRPC exporter
- [ ] Jaeger in `docker-compose` as the local collector/UI
- [ ] Service name, version, environment set as resource attributes
- [ ] Sampling: 100% in dev; a configurable ratio sampler for the "production" story. Explain the choice in your README.
- [ ] **Telemetry failure must never fail a request** — exporter errors are logged and dropped, never propagated

### Step 8.2 — Span structure

One trace per inbound request. Nested spans:

```
switchyard.request                    (root)
├── switchyard.auth
├── switchyard.ratelimit
├── switchyard.budget.check
├── switchyard.route.resolve
├── switchyard.provider.call          (one per attempt — retries create siblings)
│   ├── switchyard.breaker.check
│   └── switchyard.provider.http
├── switchyard.budget.reconcile
└── switchyard.response.write
```

- [ ] Retries appear as sibling `provider.call` spans, so a trace visually shows the retry pattern
- [ ] Fallback attempts appear as additional siblings with a `fallback=true` attribute

### Step 8.3 — Span attributes

Follow OTel semantic conventions for GenAI where they exist (`gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`) — using the standard rather than inventing your own names is a small detail that reads as professional.

- [ ] On the root span: team ID, request ID, requested model, served model, total tokens, cost (micro-dollars), fallback occurred, cache status (placeholder for Part 2)
- [ ] On provider spans: provider, model, attempt number, HTTP status, retry reason, latency
- [ ] **Never put prompt or response content in span attributes.** PII and payload size. Note this decision explicitly — it's a good signal.
- [ ] Errors recorded with `span.RecordError()` and status set, not just logged

### Step 8.4 — Context propagation

- [ ] W3C `traceparent` accepted from inbound requests and continued
- [ ] Trace context propagated into provider HTTP calls
- [ ] Trace ID injected into every `slog` line, so logs and traces correlate

### ✅ Phase 8 Checklist

- [ ] Jaeger UI shows a complete trace for a single request
- [ ] A request that retried twice then fell back shows all attempts as distinguishable spans
- [ ] Span attributes include team, models, tokens, cost — and contain **no** prompt content
- [ ] Inbound `traceparent` continues an external trace rather than starting a new one
- [ ] Trace ID appears in the corresponding log lines
- [ ] Killing the Jaeger container does not affect request success or latency
- [ ] Tracing overhead measured: p95 gateway overhead still under 10ms
- [ ] `DECISIONS.md`: sampling strategy, no-content policy, span hierarchy rationale

---

## Phase 9 — Prometheus Metrics

### Step 9.1 — Metric definitions

Be disciplined about cardinality. Team ID × model × provider × status is already a lot of series; do not add request ID or anything unbounded.

**Counters**
- [ ] `switchyard_requests_total{team, provider, model, status}`
- [ ] `switchyard_errors_total{team, provider, model, error_kind}`
- [ ] `switchyard_retries_total{provider, model, reason}`
- [ ] `switchyard_fallbacks_total{from_provider, to_provider, reason}`
- [ ] `switchyard_ratelimit_rejections_total{team, limit_type}`
- [ ] `switchyard_budget_rejections_total{team}`
- [ ] `switchyard_breaker_transitions_total{provider, model, from_state, to_state}`
- [ ] `switchyard_tokens_total{team, provider, model, direction}`

**Histograms**
- [ ] `switchyard_request_duration_seconds{provider, model}` — end-to-end
- [ ] `switchyard_gateway_overhead_seconds{}` — **your headline metric**, buckets tuned for the 1–20ms range
- [ ] `switchyard_provider_duration_seconds{provider, model}`
- [ ] `switchyard_time_to_first_token_seconds{provider, model}` — streaming only

**Gauges**
- [ ] `switchyard_provider_health{provider, model}` — 0/1/2 for down/degraded/healthy
- [ ] `switchyard_breaker_state{provider, model}` — 0/1/2 for closed/half-open/open
- [ ] `switchyard_budget_utilization_ratio{team}`
- [ ] `switchyard_ratelimit_tokens_remaining{team, limit_type}`
- [ ] `switchyard_inflight_requests{provider}`

**Cost**
- [ ] `switchyard_cost_microdollars_total{team, provider, model}` — a counter; compute rates in PromQL, don't precompute

### Step 9.2 — Wiring

- [ ] `promhttp` handler on the admin port at `/metrics`
- [ ] Metrics recorded in middleware, not scattered through business logic
- [ ] Go runtime and process collectors registered (goroutine count, GC, memory — you want these for the load test)

### Step 9.3 — Prometheus config

- [ ] `deploy/prometheus/prometheus.yml` scraping the gateway every 15s
- [ ] Recording rules for expensive queries used in dashboards (cost per team per hour, error rate by provider)
- [ ] Retention configured; volume mounted so data survives a container restart

### ✅ Phase 9 Checklist

- [ ] `/metrics` returns valid Prometheus exposition format
- [ ] Prometheus targets page shows the gateway as UP
- [ ] Every metric listed above has non-zero data after a test run
- [ ] Cardinality checked: `count({__name__=~"switchyard.*"})` is in the hundreds, not tens of thousands
- [ ] `switchyard_gateway_overhead_seconds` histogram buckets actually resolve single-digit milliseconds (not all landing in one bucket)
- [ ] Breaker and health gauges match what `/admin/providers/health` reports
- [ ] Metrics are recorded for failed requests too, not just successes
- [ ] `DECISIONS.md`: cardinality choices, counter-not-gauge for cost

---

## Phase 10 — Grafana Dashboards & Alerting

### Step 10.1 — Provisioned setup

- [ ] Grafana in `docker-compose` with the Prometheus datasource provisioned from a file
- [ ] Dashboards as JSON in `deploy/grafana/dashboards/`, auto-loaded on boot
- [ ] **No clicking things together and forgetting.** A reviewer running `docker compose up` must see populated dashboards immediately.

### Step 10.2 — Dashboard 1: Operations

- [ ] Provider health status panel (colour-coded state timeline)
- [ ] Circuit breaker state timeline
- [ ] Error rate by provider and error kind
- [ ] Fallback events over time, annotated
- [ ] Retry rate by reason
- [ ] In-flight requests by provider

### Step 10.3 — Dashboard 2: Performance

- [ ] **Gateway overhead p50/p95/p99** — put this panel top-left, it's your headline
- [ ] End-to-end latency percentiles by provider
- [ ] Provider latency percentiles (so overhead vs. provider time is visually separable)
- [ ] Time to first token, streaming
- [ ] Throughput (RPS) by provider
- [ ] Token throughput, input and output

### Step 10.4 — Dashboard 3: Business

- [ ] Cost per team per day
- [ ] Budget utilization gauges per team
- [ ] Spend by provider and model (stacked)
- [ ] Rate limit rejections by team
- [ ] Request distribution across providers
- [ ] Cost-per-request trend

### Step 10.5 — Alerting

- [ ] Grafana alert rules (file-provisioned, same as dashboards):
  - Provider error rate above 10% for 5 minutes
  - Circuit breaker opened
  - Team above 90% budget
  - Gateway overhead p99 above 25ms
  - All providers in a tier unhealthy
- [ ] Alerts route to a Slack webhook, with context: what fired, which provider/team, current value
- [ ] Alert firing verified by deliberately breaking something with the chaos endpoint

### ✅ Phase 10 Checklist

- [ ] `docker compose up` from a clean clone → Grafana loads with all three dashboards populated
- [ ] No manual Grafana configuration required anywhere
- [ ] Killing a provider is visible on the Operations dashboard within 30s
- [ ] Gateway overhead panel is the first thing you see on Performance
- [ ] Cost dashboard numbers reconcile with the provider dashboards (spot-check one team)
- [ ] At least two alerts fired and delivered to Slack during testing
- [ ] Dashboard JSON committed; screenshots saved to `docs/` for the README
- [ ] `DECISIONS.md`: dashboard split rationale, alert thresholds and why

---

## Phase 11 — Integration Testing, Load Testing & Part 1 Wrap

### Step 11.1 — Integration test suite

- [ ] Mock provider server (`httptest`) with configurable behaviour: succeed, error, slow, 429, drop
- [ ] Tests: rate limiting under concurrency is exact, not approximate
- [ ] Tests: budget blocks at the correct threshold
- [ ] Tests: retry counts and backoff timing are correct
- [ ] Tests: fallback selects the right provider, respects allowlists
- [ ] Tests: breaker opens, half-opens with exactly one probe, closes
- [ ] Tests: streaming passes through uncorrupted; client cancel propagates
- [ ] Tests: config hot reload doesn't drop in-flight requests
- [ ] `go test -race ./...` passes clean. **The race detector is non-negotiable** — you have shared state everywhere.

### Step 11.2 — Load test

- [ ] `scripts/loadtest.js` (k6) — committed, rerunnable by anyone
- [ ] Mixed workload: multiple teams, multiple models, streaming and non-streaming, some deliberately over-limit
- [ ] Target: 5,000+ requests against mock providers (don't burn real API money on load tests)
- [ ] Measure: gateway overhead p50/p95/p99, rate limit accuracy under concurrency, throughput ceiling, memory and goroutine count under sustained load
- [ ] Separate scenario: run with a provider killed mid-test, verify fallback works under load
- [ ] Results written to `docs/loadtest-results.md` with the exact command to reproduce

### Step 11.3 — Documentation

- [ ] `README.md` written as internal engineering docs, not a tutorial:
  - One-paragraph problem statement (what breaks without this)
  - Architecture diagram (Mermaid, committed as source)
  - Quickstart: `docker compose up`, then a working curl
  - Configuration reference
  - The numbers, up front: overhead p95, throughput, failover time
  - Link to `DECISIONS.md`
- [ ] `DECISIONS.md` reviewed end to end — every phase represented, every entry has a rejected alternative
- [ ] Architecture diagram shows the middleware chain order and the resilience path

### Step 11.4 — Demo scenario script

- [ ] `scripts/demo.sh` running the full story end to end:
  1. Normal request → succeeds, show trace in Jaeger
  2. Hammer rate limit → 429s appear on the dashboard
  3. Kill a provider → health degrades, breaker opens, fallback engages
  4. Restore provider → half-open probe, breaker closes
  5. Push a team over budget → 402
- [ ] Each step pauses so it can be narrated on a Loom recording

### ✅ Phase 11 Checklist — Part 1 Complete

- [ ] `go test -race ./...` passes with no races and no skipped tests
- [ ] Test coverage on `internal/ratelimit`, `internal/resilience` above 80%
- [ ] Load test completes 5,000+ requests; results committed
- [ ] **Gateway overhead p95 under 10ms under load** — if not, profile with `pprof` before moving on
- [ ] No goroutine leaks under sustained load (goroutine count returns to baseline after)
- [ ] Rate limiting accurate under 100 concurrent clients (count matches limit exactly)
- [ ] Failover verified under load, not just at rest
- [ ] Fresh clone → `docker compose up` → working system with populated dashboards, no manual steps
- [ ] `demo.sh` runs the full scenario without intervention
- [ ] README leads with numbers, all of which are reproducible from committed scripts
- [ ] `DECISIONS.md` has an entry for every phase
- [ ] **Git tag `part1-complete`**

---

## Part 1 Exit Interview

Before starting Part 2, answer these out loud, without notes. Any you can't answer is a gap to close now, not the night before an interview.

1. Why token bucket over sliding window? What does burst tolerance buy you?
2. How do you enforce a tokens-per-minute limit when output token count is unknown until after the call?
3. Redis goes down. What happens to rate limiting, and why did you choose that?
4. Why retry the primary before falling back, rather than failing over immediately?
5. Why full jitter on backoff instead of plain exponential?
6. In half-open, why one probe and not 10% of traffic?
7. Why is the breaker per provider+model rather than per provider?
8. A team's only allowed provider is down. Why doesn't SwitchYard fall back to a healthy one?
9. Where is your gateway overhead actually spent? (You should have profiled this.)
10. Mid-stream provider failure — headers are already sent. What do you do?
11. Why micro-dollar integers instead of floats for cost?
12. What's your metric cardinality, and what would blow it up?

---

## What's Deferred to Part 2

Do not build these now, even if tempted:

- Semantic cache (Redis vector similarity)
- Cost-aware routing (complexity classifier → tier mapping)
- Async quality verification loop (the thread that ties cache + routing together)
- Postgres request-log persistence
- React UI (5 screens: Overview, Playground, Live Ops, Request Logs, Usage & Cost)
- Loom walkthrough and final case-study writeup

**Scope discipline note:** if Part 1 runs over, cut Part 2's cost-aware routing before you cut anything from Phases 8–10. The observability layer is what makes this project distinct from every other portfolio gateway.
