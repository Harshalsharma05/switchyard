# Failure modes

Tier 1, Phase 3's graceful-degradation audit. Every row below was produced by
actually stopping the dependency against the full `docker compose` stack and
sending real traffic through — real Groq and Gemini calls, the real chaos
harness, the real admin API — not by reading the code and asserting what it
should do. See [Reproduce](#reproduce) to redo any of it.

`CLAUDE.md` claims the gateway must never be the reason a request fails, with
budget enforcement as the one deliberate fail-closed exception. That claim
holds under every test below. What this document adds is what the claim
doesn't say on its own: budget's fail-closed check sits *before* the
resilience machinery in the request pipeline, so a Redis outage's real blast
radius is larger, and masks more of the system, than "budget fails closed"
suggests in isolation. See [The honest single-point-of-failure
answer](#the-honest-single-point-of-failure-answer).

## Single-dependency matrix

| Dependency | Intended | Actual |
|---|---|---|
| Redis down | RPM/TPM fail open, budget fails closed, breaker uses local state, health degrades | Confirmed, but see the note below the table — the practical effect is larger than the four clauses suggest |
| Postgres down | Request logging drops rows, team auth serves from cache | Confirmed exactly |
| Jaeger/OTLP down | No effect on requests | Confirmed exactly |
| Prometheus down | No effect on requests; dashboards degrade | Requests: confirmed. Dashboards: **bug found and fixed** — `/admin/summary` hung 27s before this audit; now bounded to ~3s |
| Embedding source down | Cache disabled, requests proceed | Confirmed exactly |
| All providers down | 503 with per-candidate breakdown | Confirmed exactly |
| Quality worker backed up | No effect on requests | Confirmed by code + metrics; not saturated under live load this session (see caveat below) |

### Redis down

**What a user experiences.** Every request that isn't a plain cache hit fails
with `503 budget_check_unavailable` in well under a second (measured: 0.66s,
bounded by the 200ms-per-call Redis timeouts on RPM, TPM, and budget in
sequence). This is **not** partial degradation — since every non-cached
completion needs a budget reservation and budget always fails closed, **a full
Redis outage takes `/v1/chat/completions` to 0% success**, not "some requests
fail." A request already served from cache before the outage is unaffected;
no new request can be served from cache during the outage, because the cache
is Redis-backed too.

**What an operator sees.** Loud, structured logs at each layer:
`"RPM rate limit check failed; failing open"`, `"TPM rate limit check failed;
failing open"`, `"budget check failed; failing closed"`, and (undocumented in
the plan's own matrix, found live) `"cache degraded, serving as miss",
reason: "redis_read"`. `/healthz` and `/readyz` both stay 200 — the gateway
process itself is healthy; it is refusing billable work on purpose.

**Recovery.** Fully automatic. The moment Redis answers again, the next
request succeeds — no gateway restart, no manual reset.

### Postgres down

**What a user experiences.** A key already in the team snapshot keeps
authenticating normally. An unknown or garbage key still gets a clean 401 —
auth never fails open, Postgres up or down. Nothing about a chat completion
itself changes; the request-log write is best-effort and invisible to the
caller.

**What an operator sees.** `"team snapshot refresh failed, serving stale
teams"` logged with the snapshot's age, `switchyard_team_store_degraded`
flips to `1`, and `GET /admin/system` reports `"postgres":"down"`,
`"team_store":"degraded"`. The request-log writer logs `"writing request log
batch"` errors and drops the rows without touching the response.

**Recovery.** Automatic, bounded by the team-snapshot refresh interval
(default 30s) — no restart. A key rotated or revoked elsewhere during the
outage keeps its old behavior on this replica until that next refresh; that
window is the documented revocation lag from Tier 1 Phase 2, not a new gap.

### Jaeger/OTLP down

**What a user experiences.** Nothing. Overhead stayed at ~6.8ms on a request
sent while Jaeger was stopped — indistinguishable from Jaeger being up.

**What an operator sees.** Nothing in the gateway's own logs — OTel's
exporter failures route through `otel.SetErrorHandler`, off the request
path, and none fired during this test because the export simply never
succeeds silently in the background.

**Recovery.** Automatic; traces resume exporting once Jaeger answers again.

### Prometheus down

**What a user experiences on `/v1/chat/completions`.** Nothing — confirmed
unaffected.

**What an operator saw on `GET /admin/summary` (bug, now fixed).** The
handler issues nine independent scalar Prometheus queries plus a series
query. Before this audit they ran **sequentially**, so a down Prometheus made
each of the ten pay its own 3-second client timeout in turn — measured at
**27 seconds** to return the (correctly-shaped) degraded response. The
function's own doc comment already promised graceful, fast degradation; the
code didn't deliver it. Fixed in
[`internal/summary/summary.go`](../internal/summary/summary.go): the ten
queries now run concurrently via a `sync.WaitGroup`, with an `atomic.Bool`
for the one piece of shared state (`Degraded`) more than one goroutine
touches. Re-measured after the fix: **3.0 seconds**, bounded to one query's
timeout instead of ten, same response shape. A companion test
(`TestBuildSeriesFailureDegradesButKeepsScalars`) had an unsynchronized
counter that only stayed race-free by accident while queries ran one at a
time; it now uses `atomic.Int64`. `go test -race ./...` is clean.

**Recovery.** Automatic on both counts, and no different from before the fix
in the happy path — a live re-check with Prometheus healthy returned real
data in 20ms.

### Embedding source down

**What a user experiences.** A prompt that misses the exact-key cache tier
falls through to the semantic tier, the embedding call fails, and the
request proceeds as a plain cache miss — 200, normal latency. Tested by
pointing the cache's embedding `base_url` at an unreachable host via a
scoped config override, isolated from the Gemini *provider* slot (which kept
its real key), so this specifically exercises the embedding path and not a
provider outage.

**What an operator sees.** `"cache degraded, serving as miss", reason:
"embed"` with the underlying DNS/dial error attached.

**Recovery.** Automatic — the next request after the embedding source
recovers goes back to attempting a semantic lookup.

### All providers down

**What a user experiences.** `503 chain_exhausted` with a per-candidate
breakdown (each provider tried, its attempt count, and its error), not a
bare 503. Tested with the chaos harness forcing `error` on all three
configured providers.

**What an operator sees.** The same breakdown in the gateway log, plus each
provider's breaker opening after its own failure threshold.

**Recovery.** Automatic once any provider in the requested model's fallback
chain recovers or its breaker's cooldown elapses.

### Quality worker backed up

**What a user experiences.** Nothing, by construction: `considerQuality` runs
*after* the response is already on the wire, and `Enqueue` is a non-blocking
channel send that drops the sample with a metric
(`switchyard_quality_samples_total{outcome="dropped"}`) rather than blocking
the request goroutine on a full queue.

**Caveat, stated plainly.** This session verified the guarantee structurally
(code inspection: the non-blocking `select`/`default`, and the request path
never awaiting the worker) plus confirmed the drop-with-metric path and the
`switchyard_quality_queue_depth` gauge exist and are exposed. It did **not**
run a live saturation test — filling a 256-slot queue faster than four judge
workers can drain it needs sustained heavy concurrent load, which is a load
test, not a single-request dependency-outage probe. The load test in
[`docs/loadtest-results.md`](loadtest-results.md) already exercises this
under real concurrency and independently confirms zero request-path impact
with 223 of 284 judge calls failing under a real provider rate limit.

**Recovery.** N/A — the design has no failure state that needs recovering
from; a full queue only ever drops samples, it never accumulates unbounded
state.

## Combination scenarios

### Redis and Postgres down together

The realistic "shared infra outage" case. No new failure mode appears — the
two outages compose additively, not multiplicatively. A valid cached key
still authenticates (Postgres's own effect) and every completion still
503s at budget in the same ~0.66s bound as the Redis-only case. An unknown
key still 401s. No deadlock, no panic, `/healthz`/`/readyz` stay 200
throughout.

### Postgres down during a key rotation

Better than the plan's own framing anticipated. `POST
/admin/teams/{id}/key/rotate` is rejected with a clean `503
audit_unavailable` — the audit-log write (also Postgres-backed) is checked
*before* the key mutation is attempted, so a rotation attempt during a
Postgres outage never touches the `teams` table at all. There is no partial
state to reason about: the old key is guaranteed to still work, confirmed
live.

### Redis down during an active circuit breaker cooldown

The most consequential finding of this audit. A breaker was tripped open on
`groq` via chaos (5 forced failures, confirmed `"state":"open"` via `GET
/admin/providers/health`), then Redis was stopped mid-cooldown. The next
request came back `503 budget_check_unavailable` — **not** a breaker-specific
response. This confirms, live, that **the breaker's Redis-independent local
state (real code, unit-tested in Phase 7) is never reached by production
traffic during a full Redis outage**, because budget's fail-closed check runs
earlier in the pipeline (`Auth → RateLimit → Route → Authorize → Cache →
TPM → Budget → Resolve`) and blocks every request before `Resolve` — where
the breaker lives — ever executes. The local-state design still matters for
a brief or partial Redis blip shorter than a request's own budget-check
timeout; it just doesn't matter for a sustained outage, because nothing gets
that far.

### Provider down (chaos) and Redis down together

Same root cause, confirmed a third time with a team that has a real
fallback tier configured (groq → gemini). The request still 503s at the
budget check before `Resolve` ever runs. Fallback does not fail here — it is
never attempted. The honest answer to "does fallback still work when Redis
is down" is no, but not because the fallback logic is broken.

## The honest single-point-of-failure answer

Postgres, Jaeger, and Prometheus going down each degrade one narrow slice of
the system (team-store freshness, tracing, one admin dashboard) while
`/v1/chat/completions` keeps working. **Redis does not behave the same way.**
Because budget enforcement is unconditional, per-request, and deliberately
fail-closed — and because it runs before every piece of resilience machinery
this project is built around (retry, fallback, the circuit breaker) — a
sustained Redis outage takes the gateway's core product function to 0%
success. The rate limiter's fail-open design and the breaker's Redis-free
local state are both real, both correct, and both currently invisible in
that scenario, because nothing gets far enough to need them.

This is not a defect to silently patch around within this phase — fixing it
would mean either weakening budget's fail-closed guarantee (unacceptable per
`CLAUDE.md`: "money is not recoverable") or reordering the request pipeline
to run resilience logic before spend is verified (a design change, not a bug
fix, and out of scope for a phase that adds no features). It is the honest
answer: **Redis is a genuine single point of failure for this gateway's
billable traffic**, accepted as the cost of never letting a dollar go
unaccounted-for. Postgres and the observability stack are not.

## What the checklist confirms

- No dependency failure in any single or combination test produced a 500 on
  a request that should have succeeded — every failure was a deliberate,
  correctly-typed 401/402/503.
- Auth never failed open in any test, including both Redis and Postgres
  down simultaneously.
- Every discrepancy found was either fixed (`/admin/summary`'s sequential
  queries) or is documented above with the reasoning for leaving it as
  designed (Redis's blast radius, the breaker's masked local state).

## Reproduce

PowerShell, from `deploy/`, with the full stack already up
(`docker compose up -d --build`) and `$H = @{ Authorization = "Bearer
sk-switchyard-dev-acme-9f2b1c" }`.

```powershell
# Redis down
docker compose stop redis
irm http://localhost:8080/v1/chat/completions -Method Post -Headers $H `
  -ContentType "application/json" `
  -Body '{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}]}'
docker compose start redis

# Postgres down
docker compose stop postgres
irm http://localhost:8080/v1/chat/completions -Method Post -Headers $H `
  -ContentType "application/json" `
  -Body '{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}]}'
irm http://localhost:9090/admin/system -Headers $H
docker compose start postgres

# All providers down (chaos)
irm http://localhost:9090/admin/chaos -Method Post -Headers $H -ContentType "application/json" `
  -Body '{"rules":[{"provider":"groq","mode":"error"},{"provider":"gemini","mode":"error"},{"provider":"ollama","mode":"error"}]}'
irm http://localhost:8080/v1/chat/completions -Method Post -Headers $H `
  -ContentType "application/json" `
  -Body '{"model":"llama3.2:3b","messages":[{"role":"user","content":"hi"}]}'
irm http://localhost:9090/admin/chaos -Method Delete -Headers $H

# Redis down during an active breaker cooldown
irm http://localhost:9090/admin/chaos -Method Post -Headers $H -ContentType "application/json" `
  -Body '{"rules":[{"provider":"groq","mode":"error"}]}'
1..6 | ForEach-Object {
  irm http://localhost:8080/v1/chat/completions -Method Post -Headers $H `
    -ContentType "application/json" `
    -Body '{"model":"openai/gpt-oss-20b","messages":[{"role":"user","content":"trip breaker"}]}'
}
irm http://localhost:9090/admin/providers/health -Headers $H   # confirm breaker "open"
docker compose stop redis
irm http://localhost:8080/v1/chat/completions -Method Post -Headers $H `
  -ContentType "application/json" `
  -Body '{"model":"openai/gpt-oss-20b","messages":[{"role":"user","content":"redis down mid-cooldown"}]}'
docker compose start redis
irm http://localhost:9090/admin/chaos -Method Delete -Headers $H
irm http://localhost:9090/admin/providers/groq/breaker/reset -Method Post -Headers $H
```

Every test in this document was run against real Groq and Gemini traffic,
not mocks — timings will vary slightly run to run but the shapes (0% success
on Redis-down, clean fail-closed 503s, automatic recovery with no restart)
should reproduce exactly.
