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

Then a fifth problem appears the moment the gateway is something more than one
*person* runs: **tenancy**. A console authenticated by pasting an API key has no
concept of who is looking at it, so every screen shows everything. Tier 1 and
the multi-user work close that — real sign-in, organisations, and an
organisation filter on every query that returns tenant data — while leaving the
`:8080` request path exactly as it was.

---

## 2. What was built

| | Part 1 | Part 2 | Tier 1 + multi-user |
|---|---|---|---|
| Wire format | OpenAI-compatible `/v1/chat/completions`, streaming and not | — | — |
| Machine identity | Per-team API keys, model allowlists, YAML, hot-reloadable | `is_admin` flag | Moved to Postgres; create/delete/rotate now survive a restart *and* a reload |
| Human identity | — | Paste a team key into the console | Google OAuth → JWT in an httpOnly cookie, signed double-submit CSRF, refresh rotation with reuse detection |
| Tenancy | — | — | Organisations own projects (the renamed teams); every admin query, aggregate, audit row, and cache entry scoped from the token claim |
| Rate limiting | Token bucket, atomic Redis Lua, lazy refill, RPM + TPM | — | — |
| Budgets | Integer micro-dollars, reserve-then-reconcile, 80% warn / 100% block | Team spend UI, reconciliation against the log | Org-wide roll-up across every project |
| Failover | Health-aware, tiered fallback chains, allowlist beats availability | — | — |
| Circuit breaker | Per provider + model, one distributed-locked probe in half-open | — | — |
| Fault tolerance | — | — | Panic recovery on the request path and every long-lived goroutine; a documented failure-mode matrix produced by stopping each dependency for real |
| Observability | OTel traces, Prometheus on a separate port, provisioned Grafana | Request-log persistence (Postgres), `/admin/summary` | Audit log scoped to the tenant whose resources changed |
| Caching | — | Two-tier semantic cache (exact Redis key, then Gemini-embedded nearest-neighbour) | Keyed by organisation, with a per-project opt-out |
| Routing | — | Lexical complexity classifier → cheapest capable tier, opt-in via `model: "auto"` | — |
| Quality | — | Async LLM-as-judge on a sampled slice, off the request path entirely | — |
| UI | — | Console (Overview, Playground, Live Ops, Request Logs, Usage & Cost, Settings) behind nginx | A sign-in page; a real create-project form replacing Part 2's placeholder; a project selector scoping every view, held in the URL |

The gateway is Go 1.26, `net/http` + `chi` only — no web framework, because the
point was to show the middleware chain built by hand. State lives in Redis
(limits, budgets, breaker state, cache) and Postgres (the request log, plus
projects, users, organisations, sessions, and audit). No Kubernetes, no cloud,
no deployment at all — explicitly out of scope. Authentication *was* out of
scope until the multi-user work; the OAuth client, the JWT, and the CSRF scheme
are hand-rolled against the standard library. Those are two different arguments,
and [`DECISIONS.md`](DECISIONS.md) keeps them apart: for OAuth a library would
have been perfectly defensible and simply solved a problem this gateway doesn't
have (it never refreshes a Google token, so what remained was a `url.Values` and
one form POST), while for JWT hand-rolling is the *safer* option — a library's
value there is algorithm agility, and algorithm agility is precisely the
vulnerability. The verifier never reads the token header at all, so there is no
`alg` field for an attacker to steer it with.

---

## 3. Architecture and the constraints that shaped it

Four constraints are load-bearing. Everything else bends around them.

### "The gateway must never be the reason a request fails."

Every dependency has a written, deliberate failure mode:

| Dependency | On failure | Why |
|---|---|---|
| Redis (rate limiting) | **fail open** — log loudly, allow the request | A delayed request beats a gateway outage; a leaked quota is recoverable |
| Redis (budget) | **fail closed** — 503 `budget_check_unavailable` | Money already spent cannot be un-spent; this is the *one* deliberate exception |
| Postgres (project store) | **fail closed, from a stale snapshot** — a known key keeps working, an unknown one is 401 | Auth never fails open, and a database blip must not be a total outage |
| Telemetry (traces, metrics) | never touches the request path | Batched, async, dropped on backpressure |
| Health checker (stale) | treat every provider as healthy | Rely on passive signals from live traffic instead |
| Semantic cache (Redis, embeddings) | degrade to a miss, fire a metric | The cache is an optimisation; a slow lookup must not become latency |
| Quality worker (judge, queue) | drop the sample, zero request impact | A missed measurement is cheaper than a blocked goroutine |
| The gateway's own bugs | recover the panic, 500 that request, keep serving | Go kills the whole process on an unrecovered panic in *any* goroutine |

The load test proves the discipline holds: with the quality judge failing 223 of
284 sample calls under Groq's rate limit, `http_req_failed` stayed at **0.00%**
and overhead p95 stayed at 4 ms.

Tier 1's audit then proved the rest of it the hard way — by stopping each
dependency against the live stack and writing down what actually happened rather
than what the plan said would. [`docs/failure-modes.md`](docs/failure-modes.md)
is that record, including the one finding that was worse than intended: because
budget's fail-closed check sits *before* the resilience machinery, a full Redis
outage takes completions to **0% success**, not "some requests fail". The rate
limiter's fail-open path and the breaker's Redis-free local state are real code
that is simply never reached in that scenario. Naming that is more convincing
than the claim it complicates.

### "Overhead p95 < 10 ms, excluding provider time."

Measured from Phase 1, on every response, via `X-Switchyard-Overhead-Ms` — not
retrofitted. Part 1 landed at 2.84 ms. Part 2 put an exact-tier cache lookup and
the routing classifier on every request and it rose to **4.03 ms** — still less
than half the budget. The semantic tier's embedding call (p50 167 ms) is
excluded from the overhead number and reported on its own header, on the same
grounds provider time is excluded: it's an external round trip the gateway
doesn't control.

The budget is also why moving projects into Postgres did not become a latency
story. Authentication happens on every single request, and a database round trip
per request would have spent the whole budget on its own — so the store is never
read on the request path at all. A background ticker rebuilds the entire
in-memory registry every 30 s and swaps it behind an `atomic.Pointer`;
`Authenticate(key)` keeps its old signature and does zero I/O. The cache hit rate
is 100% by construction rather than by luck, and an unknown key is refused from
memory, so a flood of random keys can't be turned into a flood of queries. The
cost is stated rather than hidden: a revoked key stays valid for up to 30 s on
any replica that didn't make the change.

### "Streaming stays streaming."

SSE chunks flush as they arrive, never buffered — through the gateway, and
through nginx in production (`proxy_buffering off`). A cache hit for a streaming
request is replayed as chunks, not returned as one lump, so the client's
rendering code can't tell the difference.

### "Two identity types, and neither can be used as the other."

Machines authenticate with a project API key on `:8080`. Humans authenticate
with a session cookie on `:9090`. What makes this hold isn't a rule that rejects
the wrong credential — it's that neither middleware *reads* the other's. There
is no `Authorization` lookup in the session path and no cookie anywhere in
`internal/proxy`, so there is nothing to reject, and no future refactor can
"relax" a check that was never written.

That stopped being academic as soon as it was tested, because **cookies ignore
port**: a browser signed in to the console at `localhost:3001` genuinely does
attach its session cookie to a `localhost:8080` completion request in
development. The gateway has to actively not care. Both directions are
integration-tested against the real compiled binaries on both real ports rather
than asserted from the code.

The same discipline runs one level up, in the request itself: an organisation
scope is read from the verified JWT claim and **never** from a query parameter,
path segment, or body. A resource in another organisation answers **404, not
403** — a 403 would confirm that the ID is real, which is itself a disclosure
about another tenant. That rule lives at one chokepoint every `{id}` route
resolves through, so "did I remember the check" is a question answered once
rather than eight times.

### Package boundaries

`internal/provider/` knows nothing about projects, limits, or budgets — it
translates requests and calls APIs. `internal/proxy/` is the only package that
knows the full request lifecycle, and nothing imports it except `cmd/`.
Interfaces are defined by the consumer: `proxy` declares the narrow slice of the
provider registry, the rate limiter, the cache, and the judge that it actually
needs, so a test injects a fake without building the real thing.

The identity boundary is enforced the same way, by the import graph rather than
by convention: `internal/admin/` and `internal/identity/` do not import each
other at all. `identity` verifies the cookie and hands its claims to a callback;
`admin` owns its own context key; `cmd/` is the single place that knows both
exist. The two authentication systems cannot leak into each other by accident,
because there is no edge along which they could.

---

## 4. The numbers

From [`docs/loadtest-results.md`](docs/loadtest-results.md) — every figure comes
from a committed script (`scripts/loadtest.js`, `scripts/loadtest-semantic.js`,
`scripts/overheadbench`) plus the gateway's own admin API, rerunnable from a
fresh clone.

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

**Tier 1 and the multi-user work were required not to move the overhead number,
and didn't.** Both are control-plane changes by design — projects moved to
Postgres behind the snapshot described in §3, and organisation scoping happens
entirely on `:9090`. Re-measured after org scoping landed with
[`scripts/overheadbench`](scripts/overheadbench): **p95 4.59 ms** against the
10 ms budget, 2.02 ms p50 on a cache hit and 3.17 ms on a miss. Confirming it
matters more than assuming it: "the hot path was untouched" is a claim, and a
claim about latency that nobody measured is worth nothing.

**One number the plan expected to move, and the reason it didn't.** Scoping the
semantic cache by organisation was written down as a security fix that would
cost hit rate. It turned out the cache had *always* hashed the team into its
fingerprint — tighter than organisation — so there was no cross-tenant hit to
eliminate and nothing to purge. The change was a deliberate *widening* from team
to org, so that one person's several projects stop paying twice for the same
answer, with a per-project `cache_isolated` opt-out that can only narrow sharing
back. The honest consequence: the hit rate should **rise**, and the 97.2% above
is a pre-scoping measurement that has not been rerun since, so there is no
post-scoping figure to quote here. Discovering that a planned fix was already in
place — and saying so instead of taking credit for it — was the more useful
outcome.

Honest caveats, stated not buried: the load test still runs on Windows (Linux
was deferred; the monotonic clock's ~529 µs granularity makes the sub-ms tail
less trustworthy); the fallback cost delta is $0 because the mock fallback
models are priced identically to their primaries; and 60 req/s is a sustained
rate, not a measured throughput ceiling.

Tenant isolation is evidenced the same way, by attack rather than by assertion.
[`docs/tenant-isolation.md`](docs/tenant-isolation.md) stands up two real
organisations against the compiled binary over real Postgres and Redis and tries
to break out of one into the other: every endpoint by path ID, every mutation,
pagination past its own end, identical prompts through the cache in both
orderings, aggregate totals against known seeds, error bodies checked for the
other org's name, a tampered `org` claim replayed with its original signature,
and both cross-identity directions. Every one of those is now a permanent
integration test, so a change that reopens a hole fails the suite rather than a
human's memory — and the residual gaps the audit didn't reach are listed there
rather than left implied.

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

### The decision I made, shipped, and then reversed

The multi-user plan specified that superadmin actions in the audit log should be
visible to superadmins only. It was implemented exactly as written, and it was
wrong.

Combined with the rule that an entry is filed under the organisation it
*affected* rather than the actor's own, it meant a tenant could not see that an
operator had rotated its key or reset its budget — which is precisely the case
an audit log exists to prevent. Accountability runs to the affected party, not
only to the person who already knows what they did. Implementing the plan
literally produced a log that was complete for exactly one reader.

The fix was one clause: drop `AND NOT superadmin` from the read filter, so
visibility follows *which tenant was touched* rather than *who did the
touching*. The operator's identity is still withheld — a superadmin row's actor
and address are replaced with `platform-operator` on a tenant read, and that
substitution happens in the SQL rather than in the response mapper, so the real
identity never enters the process at all and a later bug in a view can't leak
what the query already replaced. A tenant needs to know its key was rotated and
that it wasn't one of its own people; it does not need the operator's email.

Both versions are in [`DECISIONS.md`](DECISIONS.md) — the original bullet is left
standing, marked superseded, with the reversal underneath it. Overwriting it
would have destroyed the more useful half of the record: the reasoning that
produced a defensible-sounding wrong answer is worth being able to recall, and a
document that only ever shows the final answer teaches nothing about how it was
reached.

**What I'd revisit, in order.**

1. **`SWITCHYARD_SUPERADMIN_EMAIL`.** Superadmin is granted by matching an
   environment variable at every sign-in, which makes a typo recoverable and
   also means whoever controls the environment controls superadmin
   *continuously*, not just at bootstrap. It is never removed automatically, so
   the flag accumulates and comes off only by hand in SQL. Fine while the
   operator and the deployer are one person; it needs to become an authenticated
   action before anyone else deploys this.
2. **Per-organisation provider keys.** Spend is attributed and capped per
   project, but every tenant's traffic still bills the operator's own Groq and
   Gemini accounts. A genuinely multi-tenant product needs bring-your-own-key,
   and this doesn't have it yet.
3. **The semantic cache's 50 ms Redis read timeout.** In the mini-run, roughly
   half the lookups that scored above the similarity threshold still missed,
   most likely because the entry fetch timed out under concurrency. The timeout
   is right in spirit — a slow cache must degrade to a miss — but 50 ms is too
   tight for the entry read specifically, as opposed to the candidate-index
   read. I'd split the two timeouts and measure again.

---

## 6. What it isn't

It runs locally. `docker compose` on one machine, no deployment, no TLS, no
hosted anything, and the numbers above come from a developer Windows box. Google
is the only way in — email and password sign-in was cut deliberately, because
two credential types on one address is an account-linking attack surface that
can't be closed safely without verified email delivery, which is its own
dependency. There is one user per organisation: no invites, no members, no roles
beyond org admin and superadmin. And revocation is not instant in either
identity system — a revoked session's access token stays valid for up to 15
minutes, a revoked project key for up to 30 seconds on a replica that didn't
make the change. Both numbers are the stated price of keeping a database round
trip off the request path, and both are written as numbers rather than implied
away.

The full list, including the residual isolation risks the audit didn't reach, is
in the README's [Known gaps](README.md#known-gaps).

---

<!-- ## 7. Exit questions

The fifteen questions in [`PART2_PLAN.md`](PART2_PLAN.md#exit-interview) are the
real test — answered out loud, from memory. The framing ones (why Postgres when
Redis and Prometheus already existed; how you know a non-identical cached answer
is right; what cache lookup cost in overhead and how you know; what stops a
non-admin calling an admin endpoint) are all answered above or one link away in
`DECISIONS.md`. -->
