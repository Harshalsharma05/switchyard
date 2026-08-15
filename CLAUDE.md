# CLAUDE.md — SwitchYard

Project instructions for Claude Code. Read this before any work in this repo.

---

## What this is

**SwitchYard** is an LLM API gateway. It sits in front of OpenAI, Anthropic, and Ollama and provides: unified request/response format, per-team authentication, rate limiting, budget enforcement, health checking, retry + fallback routing, circuit breaking, and full observability.

Part 1 (Phases 0–11) is the gateway itself. Part 2 adds a semantic cache, cost-aware routing, async quality verification, and a React UI. **Do not build Part 2 features during Part 1.** If a design decision now would make a Part 2 feature easier, say so and leave a comment — don't implement it.

The full phase-by-phase spec is in `PART1_PLAN.md`. That file is authoritative for *what* to build. This file is authoritative for *how*.

---

## Who you're working with

The repo owner is a backend engineer learning Go on this project. He has strong Python/FastAPI and async orchestration experience but is new to Go idioms.

This matters:

- **Explain Go-specific choices as you make them.** Why a pointer receiver here, why a buffered channel there, why `errors.Is` over a type assertion. One or two sentences, not a lecture.
- **Prefer clear over clever.** No reflection, no code generation, no generics unless they genuinely simplify. If there's an idiomatic-but-obscure way and a plain way, take the plain way.
- **Never dump a large multi-file change without walking through it.** Change, then explain, then wait.

---

## Rules of engagement

### Do not write these — he writes them by hand

- `internal/ratelimit/` — the token bucket implementation (Phase 3)
- `internal/resilience/breaker.go` — the circuit breaker state machine (Phase 7)

You may write the *tests* for both, and the surrounding wiring. If you find yourself about to implement either, stop and say so instead.

### Ask before

- Adding any dependency beyond what's already in `go.mod`. Name it, say what it replaces, say what the stdlib alternative costs.
- Restructuring packages or moving files between them.
- Changing anything in `configs/` schema — it has downstream effects on the admin API and hot reload.
- Making a design decision the plan doesn't specify. Present the options and the trade-off; don't silently pick.

### Always

- **State the "why" before the "what."** When you propose an approach, lead with the reasoning. If the reasoning is thin, that's a signal the approach is wrong.
- **Work one phase at a time.** Don't read ahead in `PART1_PLAN.md` and start on the next phase. Finish the checklist first.
- **Flag when something in the plan looks wrong.** The plan is not sacred. If a phase specifies something that won't work in Go or contradicts an earlier decision, say so.
- **Prefer editing existing files over creating new ones.** This repo does not need more files than the skeleton defines.

### Never

- Never add a feature that isn't in the current phase, however small.
- Never write a README, doc file, or summary unless asked. Docs come in Phase 11.
- Never silently swallow an error. Wrap it (`fmt.Errorf("doing X: %w", err)`) or handle it explicitly.
- Never use `panic` outside of `main.go` startup failures.
- Never hardcode a provider name, model name, or limit. It goes in `configs/`.

---

## Design constraints

These are load-bearing. They are the reason this project is interview-defensible. Do not optimize them away.

1. **The gateway must never be the reason a request fails.**
   Every dependency has a defined failure mode. When you touch Redis, telemetry, or the health checker, state explicitly whether it fails open or fails closed, and why. Current decisions:
   - Redis down → rate limiting **fails open** (log loudly, allow request)
   - Redis down → budget enforcement **fails closed** (money is not recoverable)
   - Telemetry down → **never blocks a request**, ever
   - Health checker stale → treat provider as healthy, rely on passive signals

2. **Gateway overhead p95 < 10ms**, excluding time spent in the provider.
   This is measured from Phase 1 onward, not retrofitted. Anything you add to the hot path must justify its latency cost. Async it or drop it.

3. **Streaming stays streaming.**
   No buffering a full response before forwarding. Chunks flush as they arrive. If a feature would require buffering, it goes async or it doesn't ship.

4. **Every number that could end up on a resume comes from a committed script.**
   Load test results, overhead measurements, failover timings — all reproducible from `scripts/`. No hand-typed figures anywhere.

---

## Stack

| Component | Choice | Constraint |
|---|---|---|
| Language | Go 1.22+ | stdlib `net/http` + `chi` router only |
| State | Redis 7 | rate limit buckets, budget counters, health state |
| Config | YAML + hot reload | `configs/providers.yaml`, `configs/teams.yaml` |
| Tracing | OpenTelemetry Go SDK | OTLP → Jaeger locally |
| Metrics | Prometheus | `promhttp` on a **separate admin port**, never the public one |
| Dashboards | Grafana | provisioned from files in `deploy/grafana/`, never clicked together |
| Providers | OpenAI, Anthropic, Ollama | Ollama is the free local fallback |
| Deploy | Docker Compose | `docker compose up` brings up everything |

**No Postgres in Part 1.** Request-log persistence arrives with the UI in Part 2. Don't add it early.

**No web framework.** If you reach for Gin, Echo, or Fiber, stop — the point of this project is showing you can build the middleware chain yourself.

---

## Package boundaries

```
cmd/gateway/          entrypoint, wiring, graceful shutdown
internal/config/      YAML loading, validation, hot reload
internal/provider/    Provider interface + OpenAI/Anthropic/Ollama impls
internal/proxy/       request handling, middleware chain, streaming
internal/auth/        team key validation, team context
internal/ratelimit/   token bucket (HAND-WRITTEN)
internal/budget/      cost accounting, spend caps
internal/health/      active + passive health checking
internal/resilience/  retry, backoff, fallback chains, circuit breaker (breaker.go HAND-WRITTEN)
internal/telemetry/   OTel setup, span helpers, Prometheus metrics
internal/admin/       admin API handlers
```

Rules:

- `internal/provider/` knows nothing about teams, limits, or budgets. It translates requests and calls APIs. That's it.
- `internal/proxy/` orchestrates. It's the only package that knows the full request lifecycle.
- Nothing imports `internal/proxy/` except `cmd/`.
- `internal/telemetry/` is imported everywhere but depends on nothing internal.
- Provider-specific quirks live behind the `Provider` interface. If a provider detail leaks into `proxy/`, the abstraction is wrong — say so.

---

## Go conventions for this repo

- **Context first.** Every function that does I/O takes `ctx context.Context` as its first parameter. No exceptions.
- **Errors wrap.** `fmt.Errorf("fetching from %s: %w", name, err)`. Sentinel errors for anything callers branch on: `var ErrBudgetExceeded = errors.New(...)`.
- **Interfaces are defined by the consumer,** not the implementer. `proxy` defines what it needs from a provider.
- **Struct-based config, not globals.** Dependencies are injected in `main.go` and passed down. No package-level mutable state.
- **Middleware signature:** `func(http.Handler) http.Handler`. The chain order is defined in one place in `proxy/` and documented there.
- **Table-driven tests.** `map[string]struct{...}` with `t.Run(name, ...)`.
- **Concurrency:** if you add a goroutine, it must have a defined lifecycle and exit path tied to a context. No fire-and-forget goroutines that outlive the request.
- **Naming:** short receiver names (`func (b *Bucket)`), no stuttering (`ratelimit.Bucket`, not `ratelimit.RateLimitBucket`).

---

## Testing

- Unit tests alongside code (`bucket_test.go` next to `bucket.go`).
- Integration tests in `test/`, tagged `//go:build integration`.
- Mock providers live in `internal/provider/mock.go` — a configurable fake that can return errors, delays, and specific status codes on demand. Build this early; every resilience test depends on it.
- Race detector is not optional: `go test -race ./...` must pass before any checklist is marked complete.
- Don't test the Go stdlib. Don't test getters. Test the state machines, the middleware chain, and the failure paths.

---

## Working rhythm

- One phase per session. `/clear` between phases.
- After each phase: run `go test -race ./...`, verify the phase checklist in `PART1_PLAN.md`, commit, tag (`git tag phase-N-complete`).
- After each phase, remind him to append to `DECISIONS.md`: what was chosen, what was rejected, why. That file is his interview prep doc.
- Commit messages: `phase-N: short imperative description`.

---

## Common failure modes to watch for

Things that will go wrong in this specific project:

- **Streaming + middleware.** Wrapping `http.ResponseWriter` breaks `http.Flusher` unless the wrapper implements it too. Check this every time a middleware wraps the writer.
- **Token bucket atomicity.** Read-modify-write against Redis across two round trips is a race. It needs a Lua script or an atomic primitive.
- **Circuit breaker half-open stampede.** If the breaker allows all requests through on half-open, a recovering provider immediately falls over again.
- **Context cancellation on fallback.** If the primary request's context is cancelled on timeout, the fallback request must get a *fresh* context or it dies instantly.
- **Cost accounting on streams.** Token counts arrive at the end of a stream, or not at all. Decide how cost is recorded for streaming requests before Phase 4.
- **Retry amplification.** Retry + fallback + client-side retry multiplies load on an already-struggling provider. Bound total attempts across the whole chain, not per-layer.

---

## Out of scope for Part 1

Say no to these, even if asked in a moment of enthusiasm:

- Semantic caching (Part 2)
- Cost-aware / complexity-based routing (Part 2)
- LLM-as-judge quality scoring (Part 2)
- React frontend (Part 2)
- Postgres, request-log persistence (Part 2)
- Kubernetes, Terraform, cloud deployment (not in scope at all)
- Auth beyond static API keys in YAML (not in scope at all)
