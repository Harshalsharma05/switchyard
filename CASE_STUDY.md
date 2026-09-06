# SwitchYard — case study

A walk through what SwitchYard is, why it is shaped the way it is, what it
measures, and the one decision worth defending hardest. [`DECISIONS.md`](DECISIONS.md)
is the exhaustive record of every choice and its alternative; this file is the
framing.

---

## 1. The problem

The moment more than one team inside a company calls LLM providers directly,
four things break at once:

1. **Attribution.** Nobody knows which team spent the money. The provider bill
   is one number.
2. **Blast radius.** One team's runaway retry loop exhausts the shared rate
   limit, and every other team starts getting 429s.
3. **Availability.** A provider outage takes down every feature that depends on
   it, with no automatic path to a working alternative.
4. **Observability.** "Why was that request slow?" has no answer, because no
   trace spans the call.

The naive fix — a shared client library — solves none of these, because a
library runs in each service's process and can't enforce a global limit,
attribute spend it doesn't see settled, or fail over on policy it doesn't own.
The controls have to sit *between* the services and the providers, on the
request path.

SwitchYard is that gateway. It speaks OpenAI's wire format, so adopting it is a
one-line base-URL change, and it adds **~4 ms** to the request path to do all
four jobs.

Part 2 adds three optimisations on top — a semantic cache, cost-aware routing,
and asynchronous quality verification — and a React console over the whole
thing.

---

## 2. What was built

| | Part 1 | Part 2 |
|---|---|---|
| Wire format | OpenAI-compatible `/v1/chat/completions`, streaming and not | — |
| Identity | Per-team API keys, model allowlists, YAML, hot-reloadable | `is_admin` flag; `GET /admin/me` |
| Rate limiting | Token bucket, atomic Redis Lua, lazy refill, RPM + TPM | — |
| Budgets | Integer micro-dollars, reserve-then-reconcile, 80% warn / 100% block | Team spend UI, reconciliation against the log |
| Failover | Health-aware, tiered fallback chains, allowlist beats availability | — |
| Circuit breaker | Per provider + model, one distributed-locked probe in half-open | — |
| Observability | OTel traces, Prometheus on a separate port, provisioned Grafana | Request-log persistence (Postgres), `/admin/summary` |
| Caching | — | Two-tier semantic cache (exact Redis key, then Gemini-embedded nearest-neighbour) |
| Routing | — | Lexical complexity classifier → cheapest capable tier, opt-in via `model: "auto"` |
| Quality | — | Async LLM-as-judge on a sampled slice, off the request path entirely |
| UI | — | Five-screen console (Overview, Playground, Live Ops, Request Logs, Usage & Cost) behind nginx |

The gateway is Go 1.26, `net/http` + `chi` only — no web framework, because the
point was to show the middleware chain built by hand. State lives in Redis
(limits, budgets, breaker state, cache) and Postgres (the request log). No
Kubernetes, no cloud, no auth beyond static keys — all explicitly out of scope.

---

## 3. Architecture and the constraints that shaped it

Three constraints are load-bearing. Everything else bends around them.

### "The gateway must never be the reason a request fails."

Every dependency has a written, deliberate failure mode:

| Dependency | On failure | Why |
|---|---|---|
| Redis (rate limiting) | **fail open** — log loudly, allow the request | A delayed request beats a gateway outage; a leaked quota is recoverable |
| Redis (budget) | **fail closed** — 503 `budget_check_unavailable` | Money already spent cannot be un-spent; this is the *one* deliberate exception |
| Telemetry (traces, metrics) | never touches the request path | Batched, async, dropped on backpressure |
| Health checker (stale) | treat every provider as healthy | Rely on passive signals from live traffic instead |
| Semantic cache (Redis, embeddings) | degrade to a miss, fire a metric | The cache is an optimisation; a slow lookup must not become latency |
| Quality worker (judge, queue) | drop the sample, zero request impact | A missed measurement is cheaper than a blocked goroutine |

The load test proves the discipline holds: with the quality judge failing 223 of
284 sample calls under Groq's rate limit, `http_req_failed` stayed at **0.00%**
and overhead p95 stayed at 4 ms.

### "Overhead p95 < 10 ms, excluding provider time."

Measured from Phase 1, on every response, via `X-Switchyard-Overhead-Ms` — not
retrofitted. Part 1 landed at 2.84 ms. Part 2 put an exact-tier cache lookup and
the routing classifier on every request and it rose to **4.03 ms** — still less
than half the budget. The semantic tier's embedding call (p50 167 ms) is
excluded from the overhead number and reported on its own header, on the same
grounds provider time is excluded: it's an external round trip the gateway
doesn't control.

### "Streaming stays streaming."

SSE chunks flush as they arrive, never buffered — through the gateway, and
through nginx in production (`proxy_buffering off`). A cache hit for a streaming
request is replayed as chunks, not returned as one lump, so the client's
rendering code can't tell the difference.

### Package boundaries

`internal/provider/` knows nothing about teams, limits, or budgets — it
translates requests and calls APIs. `internal/proxy/` is the only package that
knows the full request lifecycle, and nothing imports it except `cmd/`.
Interfaces are defined by the consumer: `proxy` declares the narrow slice of the
provider registry, the rate limiter, the cache, and the judge that it actually
needs, so a test injects a fake without building the real thing.

---

## 4. The numbers

From [`docs/loadtest-results.md`](docs/loadtest-results.md) — every figure comes
from a committed script (`scripts/loadtest.js`, `scripts/loadtest-semantic.js`)
plus the gateway's own admin API, rerunnable from a fresh clone.

| Metric | Part 1 | Part 2 |
|---|---|---|
| Gateway overhead p50 / p95 / p99 | 1.57 / 2.84 / — ms | 1.87 / 4.03 / 8.98 ms |
| Requests / rate / duration | 5,400 / 60 req/s / 90 s | 5,403 / 60 req/s / 90 s |
| `http_req_failed` (genuine failures) | metric was misleading | **0.00%** |
| Rate-limit rejections (429) | 1,594 | 1,606 |
| Budget denials (402) | 530 | 147 (from a clean monthly counter) |
| Fallback-served during the outage | 1,118 | 781 |
| Cache slice hit rate | — | 97.2% (hits ~2 ms e2e vs misses ~35 ms) |
| Cost saved by cache | — | $0.0250 over 834 hits |
| Cost saved by routing | — | $0.0249 over 277 downgrades |
| Redis spend vs request-log sum | — | exact agreement, all three teams |
| Semantic embedding call p50 / p95 | — | 167 / 464 ms (real Gemini) |

Two Part 1 reporting bugs were fixed at the source in this rerun: the k6
`--summary-export` quirk that wrote `p(95)<10: false` on a passing run (now
computed directly in `handleSummary()`), and `http_req_failed` being inflated by
the run's deliberate 429/402/503 (now scoped with `expectedStatuses`).

Honest caveats, stated not buried: the load test still runs on Windows (Linux
was deferred; the monotonic clock's ~529 µs granularity makes the sub-ms tail
less trustworthy); the fallback cost delta is $0 because the mock fallback
models are priced identically to their primaries; and 60 req/s is a sustained
rate, not a measured throughput ceiling.

---

## 5. The decision I'd defend hardest

**Budget enforcement fails *closed* while everything else fails open — and that
asymmetry is the whole design in miniature.**

The rule "the gateway must never be the reason a request fails" is absolute
everywhere except here. When Redis is unreachable, a rate-limit check that can't
run lets the request through: the cost is a team briefly exceeding its quota,
which is recoverable, and the alternative — refusing traffic because a
*telemetry-adjacent* dependency hiccuped — is exactly the gateway becoming the
outage. But a budget check that can't run returns a 503, because a dollar spent
past a cap cannot be clawed back, and a team that has genuinely hit its limit
getting one more expensive request through is a worse outcome than a request
that fails and can be retried in a second.

The 503 is deliberate too: not a 402. A 402 asserts the specific business fact
"this team is over budget," which was never actually verified — the check
couldn't run. 503 says what's true: the check itself is unavailable. Getting
that distinction right is the difference between an honest error and a
misleading one.

I'd defend it because it shows the project isn't cargo-culting a slogan. "Never
fail a request" is easy to say and wrong in exactly one place, and finding that
place — and writing down *why* it's the exception — is the actual engineering.

**What I'd revisit:** the semantic cache's 50 ms Redis read timeout. In the
mini-run, roughly half the lookups that scored above the similarity threshold
still missed, most likely because the entry fetch timed out under concurrency.
The timeout is right in spirit — a slow cache must degrade to a miss — but 50 ms
is too tight for the entry read specifically, as opposed to the candidate-index
read. I'd split the two timeouts and measure again.

---

<!-- ## 6. Exit questions

The fifteen questions in [`PART2_PLAN.md`](PART2_PLAN.md#exit-interview) are the
real test — answered out loud, from memory. The framing ones (why Postgres when
Redis and Prometheus already existed; how you know a non-identical cached answer
is right; what cache lookup cost in overhead and how you know; what stops a
non-admin calling an admin endpoint) are all answered above or one link away in
`DECISIONS.md`. -->
