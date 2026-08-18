# DECISIONS

What was chosen, what was rejected, why. One section per phase.

---

## Phase 1 — Provider Abstraction & Core Proxy

### Public wire format: mirror OpenAI

Chosen over a bespoke SwitchYard schema. Clients adopt the gateway by changing one base URL and every existing SDK works unmodified — a materially stronger demo than a format nobody speaks. Cost accepted: we inherit OpenAI's warts.

Guardrail: the public wire DTO lives in `proxy/` as a *separate struct* from `provider.Request`. If they ever merge, OpenAI has silently become the internal format and every other adapter gets uglier.

### Error taxonomy: typed `Kind`, stored `Retryable`

Chosen over passing raw HTTP status codes up the stack. Retry, fallback, and the breaker must branch on *meaning*, not on a number whose interpretation differs per provider — status codes would force provider knowledge into `proxy/`.

- **`Retryable` is stored, not derived from `Kind` at read time.** The adapter has read the response body; the retry layer has not. A 429 from a soft rate limit is retryable, a 429 from an exhausted billing quota is not, and both arrive as the same status. `Kind` supplies the default via `DefaultRetryable()`; the adapter overrides when the body says more.
- **Added `KindInvalidRequest`, which the plan omits.** Without it a provider 400 (malformed body, unknown model, context overflow) lands in `KindServerError`, which defaults to retryable — so Phase 6 would retry a request that can never succeed, against every provider in the chain.
- **`Kind` is a named string, not `iota`.** It becomes a Prometheus label in Phase 9 with no conversion, and its zero value is `""` rather than a valid classification, so an unset `Kind` reads as a bug instead of silently meaning "rate limited".

### System prompt stays a `Message`, not a `Request` field

Rejected a top-level `Request.System string`. Anthropic and Gemini want it top-level; OpenAI and Ollama want it in the array. Putting it on the canonical type makes the majority special-case it, so the adapters that need it hoist it themselves. Edge case: multiple system messages, or one out of first position, are concatenated in order.

### `Provider` interface lives in `provider`, not the consumer

Departs from this repo's consumer-defines-the-interface rule. The registry returns a `Provider` and lives in `provider`, so a `proxy.Provider` would create a `provider` ↔ `proxy` import cycle that Go rejects at compile time. Go's implicit satisfaction means `proxy` can still declare a narrower interface later with no adapter changes.

### Streaming types declared in Phase 1, implemented in Phase 2

`StreamReader` and `Chunk` declared now; `Stream` returns `ErrStreamingNotImplemented`. Same reasoning the plan gives for `Ping` on day one — the interface is final, so Phase 2 never reopens an adapter file to change a signature.

### Dev stand-ins: Groq for the OpenAI slot, Gemini for the Anthropic slot

OpenAI/Anthropic slots backed by Groq/Gemini during dev due to cost; interface never assumed a specific vendor, so swapping in real keys before the demo is a config change, not a code change.

Rejected using an OpenAI-compatible free service for *both* slots. Gemini differs from OpenAI in the same places Anthropic does — top-level system instruction, different usage and finish-reason vocabulary — so building it proves the adapters are genuinely decoupled rather than all secretly OpenAI-shaped.

Consequence: `providers.yaml` separates instance `name` (`groq`, `openai`) from adapter `type` (`openai-compatible`). No provider name is hardcoded in Go.

### YAML parser: `goccy/go-yaml`

Chosen over `yaml.v3` (archived upstream, no further fixes) and `sigs.k8s.io/yaml` (pulls in `yaml.v2` transitively anyway). `goccy` reports errors with line, column, and a source excerpt, which is what makes "malformed config prevents startup with a clear message" actually true rather than a bare `yaml: line 6: ...`.

### `enabled` flag on each provider entry

Added to `providers.yaml`, not in the plan. Without it, a provider entry with no key set (OpenAI, Anthropic — no paid key yet) either hard-fails startup or has to be deleted, losing the documentation of intent. A disabled entry is still validated and its model names still reserved, so enabling it later can't introduce a collision that was invisible while it was off.

### Cost stored as integer micro-dollars, converted once at load

YAML holds readable decimals (`0.59`); `internal/config` converts to `int64` micro-dollars exactly once, at load. Phase 4 accumulates over thousands of requests, and repeated float addition drifts — the float exists for one multiplication and never again.

### Upstream auth failure maps to 502, not 401

A rejected upstream credential is reported to the caller as 502, not 401. A 401 says *their* key was rejected, which they can't act on and would waste time rotating a key that was never the problem. From the client's seat, a bad upstream credential is a broken gateway.

### Per-provider timeout applied via context, not `http.Client.Timeout`

Each adapter wraps the caller's `ctx` with `context.WithTimeout(ctx, cfg.Timeout)`, so whichever deadline is sooner wins automatically. `Client.Timeout` would silently override a shorter caller deadline, which Phase 6's "never retry past the caller's deadline" rule depends on.

### Gateway overhead measurement: known limitation on Windows dev machine

`X-Switchyard-Overhead-Ms` = handler time minus `provider.Response.Latency` (the adapter's own measured round trip), so adapter translation work correctly counts as overhead. Verified live against real Groq and Gemini calls. However, Go's monotonic clock on this Windows dev machine has ~529µs granularity — sub-millisecond overhead often reads as `0.000`. Not a code bug, but it means the sub-10ms number can't be credibly measured on Windows; Phase 11's load test must run inside Docker (Linux), where clock resolution is nanosecond-grade.

---

## Phase 2 — Streaming Passthrough

### Wire format: separate `chatChunkResponse`, no usage on the wire

- Mirrors OpenAI's `chat.completion.chunk` shape, kept as its own DTO from `chatResponse` — same reason request/response DTOs are split from the canonical types.
- `role` sent once, on the first forwarded chunk only; `finish_reason` is `*string` so it's JSON `null` until the last chunk, not `""`.
- Token usage is **not** put on the wire in Part 1 (no client `stream_options.include_usage` support yet). Still captured server-side into `requestMetrics.usage` and logged, satisfying the plan's "token counts logged correctly for streaming requests" without a wire change.

### Provider overhead for a stream = time to first byte

- `metrics.providerTime` is set once, on the stream's first `Recv()` outcome (chunk, EOF, or error) — not when `Stream()` returns.
- Status/headers are never sent eagerly; the first real client `Write()` is the first forwarded chunk, which is also what `headerHook` (Phase 1) uses to timestamp the overhead header. Net effect: `X-Switchyard-Overhead-Ms` on a stream reads as time-to-first-token, the boundary the plan recommends.

### Mid-stream error semantics: status code before the first byte, SSE event after

- Tracked with one `sentAny bool`. `Stream()` failing, or any `Recv()` failing before a chunk has actually been written to the client, gets a normal status-coded error (`writeProviderError`) — nothing is on the wire yet, so the status line can still change.
- A failure after at least one chunk was written gets an SSE `data:` event (`writeSSEError`, reusing the same `errorBody` envelope) since the status line is already committed. No `[DONE]` follows an error.
- **No fallback on a mid-stream failure.** Partial content already reached the client; retrying against another provider would duplicate it, not fix it. (Matches the plan's recommended answer, recorded here for when Phase 6 exists.)

### Client disconnect cancels the upstream call for free

- `r.Context()` is passed straight into `prov.Stream()`; net/http already cancels a request's `Context` when the client's connection closes. No extra wiring, no polling — verified with a test that cancels the client-side context and confirms the mock's `Recv()` observes it.
- `openStream` (the streaming HTTP helper) deliberately does **not** wrap `ctx` in the provider's `cfg.Timeout` the way the non-streaming `postJSON` does. That timeout is sized for one request/response round trip; a healthy stream can legitimately run far longer. The caller's own context is the only cancellation signal for a stream.

### Content-free chunks are filtered, not forwarded

- Found live against Groq's reasoning model: it sends dozens of chunks with empty content and no finish reason before real text starts. Forwarding each as its own SSE event was technically correct but needlessly noisy (72 events for a ten-line answer).
- Since usage is never put on the wire, a chunk with no content and no finish reason carries nothing a client can use — now skipped. Cut the same response to 21 events with identical information content.

### SSE parsing shared across dialects; NDJSON kept separate

- One `sseReader`/`sseStreamReader` pair (in `provider/sse.go`) drives OpenAI, Anthropic, and Gemini — their event *framing* is identical (`event:`/`data:`, blank-line delimited); only what's inside `data:` differs, so each adapter supplies just a `decode` closure.
- Ollama's NDJSON is a genuinely different wire format (one JSON object per line, no SSE framing) and gets its own `ndjsonStreamReader` rather than being forced through the SSE abstraction.

### Anthropic stream usage: input and output tokens arrive a whole stream apart

- Input tokens come once, at `message_start`; output tokens come later, at `message_delta`. `newStreamDecoder` returns a closure that closes over `inputTokens` to combine both into one `Usage` when `message_delta` arrives — the only adapter that needs stream-local state to decode correctly.

---

## Phase 3 — Auth & Rate Limiting

### Step 3.1 — Team config & authentication

- **SHA-256, not bcrypt, for API keys.** This hash runs on every request, not once at signup; bcrypt's deliberate slowness is right for a password but would put tens of milliseconds on a path this project measures in single digits. A leaked team key grants API quota under that team's own limits, not account access, so the lower threat model doesn't need bcrypt's cost.
- **`configs/teams.yaml` stores the hash, never the plaintext key.** The file is committed to git; only a SHA-256 digest may live there. The plaintext exists only in the caller's own request and is never persisted.
- **Auth middleware answers "who," not "what they can do."** It resolves a bearer token to a `*auth.Team` and attaches it to the context — nothing more. The model-allowlist 403 check lives in `handler.go` instead, because it needs the decoded request body, and nothing before the handler has parsed one.
- **Router bug caught before it shipped: `Logger` must wrap `Auth`, not the other way round.** The original chain sketch put `Auth` between `Timing` and `Logger`. Since a 401 rejection returns without calling `next`, anything placed *inside* `Auth` — including `Logger` — would never run for a rejected request, so 401s would produce zero log lines. Fixed by moving `Logger` outside `Auth`.
- **Rate limit / budget / priority fields are parsed and validated now, enforced starting Step 3.2 onward.** Same reasoning as `Provider.Ping` being declared in Phase 1 before Phase 5's health checker exists to call it — the schema doesn't get reopened when the enforcement phase arrives.
- **Duplicate-hash detection lives in both `config.LoadTeams` and `auth.NewRegistry`.** Mirrors `provider.NewRegistry`'s own duplicate-name check existing alongside `LoadProviders`' — defense in depth: the registry's own invariant should hold even if something other than the YAML loader ever constructs one.

### Step 3.2 — Token bucket

- **Token bucket over sliding window or leaky bucket: burst tolerance.** A team that has been idle accrues capacity and can legitimately spend it in one burst — a batch job kicking off, a user pasting a long conversation. A sliding or fixed window would flatten that burst into an average rate and reject traffic a reasonable client would expect to succeed; a leaky bucket enforces a *constant* output rate and would smooth out the same legitimate burst into an artificial queue. Token bucket is the only one of the three whose whole point is "accrued idle capacity can be spent in a burst, then it's gone."
- **One Lua script, not a Go read-modify-write.** Redis runs a script to completion without interleaving any other command, so two gateway replicas hitting the same key can't both read the same starting balance and both admit a request only one of them should have. A GET-then-SET across two round trips is exactly that race.
- **Lazy refill, no background goroutine.** Every call recomputes the balance from `elapsed = now - stored_ts`, so a bucket untouched for an hour needs no catching up — the next call just sees a large `elapsed`. This is also what makes the algorithm replica-safe with zero coordination beyond Redis itself: there's no timer state anywhere to drift or duplicate across replicas.
- **Time comes from Redis's own `TIME` command, not the caller's wall clock.** Replicas on hosts with clocks even slightly apart would each compute a different `elapsed` against the same stored timestamp. Redis is the one clock every replica already agrees on, for free.
- **Key schema `switchyard:rl:{team_id}:{rpm|tpm}`, expiry via `PEXPIRE` in milliseconds, not `EXPIRE` in seconds.** Caught live: converting a sub-second TTL to whole seconds truncates to `0`, and `EXPIRE key 0` deletes the key immediately instead of leaving it alone — a bucket could vanish the instant it was written.
- **Two independent buckets per team, not one.** A request can be within its RPM budget and still be too expensive in tokens, or vice versa — collapsing them into one bucket would let either dimension silently cap the other.

### Step 3.3 — TPM reservation

- **Reserve a ceiling up front, reconcile after.** The TPM bucket is charged `estimated_input + max_tokens` before the provider call; the unused portion is returned once real usage is known. The reservation always exceeds the eventual truth, so a request is never under-charged while in flight.
- **Rejected: bill only after the response.** Output tokens aren't known until completion, so a team could exceed its TPM by an unbounded margin inside one burst, with the charge landing only after the damage was done.
- **Rejected: meter input tokens only.** Exact and cheap, but leaves the expensive half unmetered — output is where both the cost and the capacity actually go.
- **Reconciliation is signed, not refund-only.** `Reconcile` applies `reserved - actual`: positive returns tokens, negative debits an estimate that came in too low. One Lua script covers both directions.
- **Refunds cap at capacity; debits may go negative.** Capping the top stops repeated over-reservation becoming burst credit. Allowing a negative balance makes a team that overshot wait out the deficit rather than have it forgiven — the refill and retry-after math both handle negative balances unchanged.
- **A refund to an expired key recreates it full.** A bucket with no stored state is by definition full, so there is nothing to return to and nothing lost.
- **`defer` owns the reconcile.** Registered immediately after a successful reserve, so an error, a panic, or a client disconnect all still return the reservation on the way out — the failure case Step 3.3 calls out specifically.
- **A denied reserve yields a nil `*Reservation` that reconciles as a no-op.** Nothing was taken, so nothing is owed back; the nil-receiver method keeps that from becoming a nil check at every call site.
- **Estimator is `bytes/4 + 4 per message`, not a real tokenizer.** Rejected `tiktoken` and friends: a separate dependency per provider dialect, to buy precision the reconcile step supplies anyway. Bytes rather than runes because `bytes/4` lands closer for multi-byte scripts.
- **The output ceiling is a parameter, not a constant.** Every adapter substitutes its instance's `DefaultMaxTokens` when a caller omits `max_tokens`, and `proxy` can't reach that through the `Provider` interface — so the caller supplies it, keeping the limit in `configs/` per the no-hardcoded-limits rule. Step 3.4 wires where it comes from.
- **TPM enforcement lands in the handler, not the middleware.** It needs the decoded body to estimate, and decoding happens in `ChatCompletions` — the same constraint that put the Step 3.1 model-allowlist check there. RPM needs no body and can stay middleware.

### Step 3.4 — Rate limit middleware & responses

- **Split enforcement: RPM in middleware, TPM in the handler.** Consequence of the 3.3 decision above — one "rate limit middleware" doesn't exist as a single unit; `RateLimit` (middleware) and `reserveTokens` (handler.go) share the same 429 response helper instead.
- **`provider.Registry` gained `DefaultMaxTokensFor(model)`.** The TPM estimate needs the serving instance's configured ceiling, which `Provider` doesn't expose and `proxy` can't otherwise reach. Rejected adding the method to `Provider` itself — that interface is deliberately small, and the ceiling is already indexed per-model inside `Registry` the same way `byModel` is, so extending it there touches zero adapter files.
- **Redis-down fail-open confirmed live, not just asserted.** Stopping Redis mid-session still returned 200s from real Ollama traffic. Caught two real bugs doing this: `docker compose stop` doesn't tear down the host port immediately (SIGTERM grace period), so a "Redis is down" test against it can silently pass against a still-half-alive Redis — had to point at a guaranteed-empty port instead.
- **Fail-open took 3.5s before it was bounded.** go-redis's default dial/retry behavior turns "Redis unreachable" into several seconds of added latency — a slow fail-open is still the gateway being the reason a request is slow. Fixed with a `context.WithTimeout(200ms)` at each call site (`RateLimit` middleware, `reserveTokens`), which is the one mechanism a Go Redis client is guaranteed to respect regardless of its internal retry behavior. Tightened `redis.Options` (150ms dial/read/write, `MaxRetries: 1`) too, but the context deadline is the actual ceiling.
- **`Reconcile`'s settle-up context is its own, not the request's.** A client disconnect or request timeout cancels `r.Context()`; reconciling on that context would fail before the Redis call could run, silently leaking the reservation until its TTL expired. `context.WithTimeout(context.Background(), 2s)` instead, called from a `defer` registered right after a successful reserve.
- **`X-RateLimit-Reset` = seconds to full refill, not a fixed-window timestamp.** A token bucket has no discrete reset point; "time until this team could make its largest possible request again" is the closest honest analog.
- **`X-RateLimit-Remaining` floors at zero for display.** The bucket itself can go negative (Step 3.3's overage debit), but a client-facing count showing a debt reads as a bug, not a feature.
- **`Retry-After` rounds up, not down.** A truncated-down wait would advertise a retry time still inside the denial window, guaranteeing a second 429.

### Step 3.5 — Priority tiers

- **No priority queue, no shared pool.** Per the plan's own instruction. This reads a team's own bucket state and a fixed 80%/20% line — nothing coordinated across teams, nothing that reorders concurrent requests.
- **Threshold checked on the bucket state *after* this request was admitted, not reconstructed to before it.** Two equally defensible readings existed; this one treats the request that crosses the 80% line as the one that gets shed, which is the simpler of the two and needs no extra arithmetic to undo Consume's already-applied deduction.
- **RPM shed does not refund; TPM shed does.** RPM's unit cost is always exactly 1 — refunding it would erase the backpressure and make an immediate retry look free. TPM's reservation is a *ceiling* (estimated input + max_tokens) that can be far larger than 1; nothing was generated and no provider was called, so real usage is unambiguously zero, and leaving the full ceiling consumed would overcharge a request that never got as far as the ones that normally reconcile against real usage.
- **The refund gets its own context, not the one already spent on the check.** `checkCtx` is budgeted to 200ms for the Reserve call; reusing it for the follow-up Reconcile risked the refund losing the race against its own deadline. Reconcile gets the same standalone `reconcileTimeout` context the deferred settle-up in `ChatCompletions` already uses.
- **Shed responses reuse the 429 header contract but a distinct `priority_shed` error type.** Same `Retry-After`/`X-RateLimit-*` shape as hard exhaustion, so clients handle both with one code path, but the message says why: capacity existed, the team was just deprioritized.
- **Verified against real Redis and real Ollama, not just the stub.** Primed a batch team's TPM bucket to 10% and confirmed the shed *and* the refund (balance returned to ~pre-request); primed a realtime team identically and confirmed it went through — same threshold, opposite outcome, live.

---

## Phase 4 — Budget Enforcement & Admin API

### Step 4.1 — Cost accounting

- **`internal/budget` is a new package, not new code inside `proxy`.** CLAUDE.md's package table already assigns "cost accounting, spend caps" to it, and Phase 1's `providers.go` comment ("Phase 4's budget accounting reads pricing from here instead") anticipated the split. `budget.Calculator` knows nothing about HTTP, teams, or providers — it only turns `(model, tokens)` into micro-dollars.
- **`budget.Pricing` is its own type, not a reuse of `config.ModelPricing`.** Same reasoning that already kept pricing out of `provider.Config` in Phase 1: `budget` shouldn't know how a price was loaded, only what it is. `cmd/gateway/main.go` converts `config.Providers.Pricing` into `budget.Pricing` once, at wiring time — the same place every other cross-package type gets assembled.
- **`proxy.CostCalculator` is a consumer-defined interface**, mirroring `Resolver` and `RateLimiter`: the handler depends on `Cost(model, in, out) (int64, error)`, not on a concrete `*budget.Calculator`, so tests inject a stub instead of building a real pricing table.
- **Cost is computed in `handler.go`, at the same two points `usage` becomes final** — the non-streaming success path and the streaming clean-completion path — rather than inside `requestMetrics` itself. `requestMetrics` stays a passive struct with no dependencies; only the handler holds the `Calculator`.
- **A pricing-lookup failure never fails the request.** By the time `recordCost` runs, the response has already succeeded (or the stream has already completed); the failure is logged and the cost stays zero. Same rule CLAUDE.md states for telemetry — bookkeeping must never become the reason a request fails.
- **An unpriced model is an error, not a silent zero.** Every model the registry can resolve has a pricing entry by construction (`internal/config` populates both from the same `providers.yaml` entry in one pass), so a miss means the `Calculator` was built from a different pricing table than the registry it's paired with — a wiring bug worth surfacing in logs, not a real gap to shrug off as free.
- **Truncating division, not rounding, on the final `/ 1_000_000`.** At micro-dollar resolution the discarded remainder is under one-millionth of a dollar per request — six orders of magnitude below a cent. What actually matters, per CLAUDE.md's rule that cost is never a float, is that every step of the computation stays `int64` throughout; truncation vs. rounding isn't a meaningful choice at this scale.
- **Known, accepted gap: a mid-stream provider failure prices at zero.** No terminal usage-bearing chunk ever arrives in that case, so `recordCost` is never reached on that exit path. The Step 3.3 byte-based estimator (`bytes/4 + 4/message`) exists to size a TPM reservation, not to bill accurately — extending it to cost would trade an honest zero for a guess dressed up as a real number. Revisit only if mid-stream failures turn out to be frequent enough to matter financially; Part 1 accepts undercounting the rare case over fabricating precision.

### Step 4.2 — Budget tracking

- **A single Lua script (`reserveScript`), not plain `INCRBY` plus a separate conditional `DECRBY`.** The plan's own wording — "INCRBY in micro-dollars — atomic, no read-modify-write race" — was presented as a real fork with a trade-off, not silently picked: a bare `INCRBY`-then-check-then-rollback needs 2–3 Redis round trips and leaves a sliver of time where the counter reads over-cap before the rollback lands. One script does the increment, the cap comparison, the rollback, and the first-time TTL inside a single atomic Redis-side execution — always exactly one round trip, and no other command can ever observe the counter holding an over-cap value, even briefly. This mirrors `ratelimit.consumeScript` in spirit (one atomic decision, one round trip) but needs none of its refill math, since a monthly counter has no lazy decay — there is nothing here but an increment and a comparison.
- **`Reconcile` needs no script of its own.** Settling a reservation is one signed `INCRBY` with no cap re-check — there is no read to race against, only one atomic write, unlike `Reserve`'s admit-or-deny decision. This mirrors `ratelimit.Reservation.Reconcile`'s own reasoning for not re-deriving admission at settle-up time.
- **Budget fails *closed* on a Redis error; rate limiting fails open.** This is the one deliberate exception to "the gateway must never be the reason a request fails," called out explicitly in CLAUDE.md: money already spent cannot be un-spent, so an unverifiable cap blocks the request rather than letting it through blind. The failure is reported as 503 `budget_check_unavailable`, not 402 — a 402 asserts the specific business fact "this team is over budget," which was never actually verified; 503 says what's true, that the check itself couldn't run.
- **Reserve-then-reconcile against the *estimated max* cost, mirroring Step 3.3's TPM pattern exactly.** The estimate prices `estimateInputTokens()` plus the output ceiling (`req.MaxTokens` or the provider's `DefaultMaxTokens`) — pulled out into its own `estimateOutputCeiling` helper so Step 3.3's TPM estimate and this one price the same ceiling without duplicating the "which ceiling applies" logic. Real usage is unknown until the response lands, so the reservation is deliberately sized to the worst case a request could cost, the same reasoning that already justified TPM's up-front reservation.
- **The 80% warning threshold lives in `proxy`, not `internal/budget`.** `budget.Tracker` enforces only the hard 100% cap, atomically, inside `Reserve`. Whether to attach a warning header and what ratio triggers it is a response-shaping decision, not a spend-tracking one — the same split Step 3.5 already draws between `ratelimit`'s raw bucket state and `proxy`'s own `batchShedFloor`.
- **The budget check is not a middleware layer, for the same reason TPM isn't.** It needs the decoded body to estimate a cost, which nothing before the handler has parsed, so `reserveBudget` lives in `handler.go` and runs right after the TPM reservation succeeds — same shape as `reserveTokens`, same nil-safe `*budget.Reservation` handed to a `defer`red `Reconcile`.
- **The 402 body reports both the cap and the current spend in dollars, not micro-dollars.** Headers and logs keep the raw integer for precision; the one place this project puts a cost in front of a person reads better as `$9.50 of $10.00` than as `9500000 of 10000000`.
- **`utilization` and `formatUSD` are tested as pure functions, independent of the HTTP-level tests.** The HTTP-level tests (denied, fails-closed, warning header present/absent) prove the wiring; the pure-function tests pin the arithmetic itself down without needing a fake server to exercise a rounding edge case.

### Step 4.3 — Admin API

- **`GET /admin/teams` reports configured RPM/TPM, not live bucket occupancy — a real trade-off, not a silent scope cut.** "Current rate limit state" could mean either. Live remaining capacity would need a read-only `Peek` on `ratelimit.Limiter`, but `bucket.go` is the file CLAUDE.md has him writing by hand, and there is no way to read a bucket's balance without either reimplementing the refill math outside the package (duplicating logic he owns) or adding a method to that file directly. Presented as an explicit choice rather than picked silently: build `Peek` himself and wire it in later, or ship configured-limits-plus-live-spend now. He chose the latter. Live occupancy stays a follow-up once `Peek` exists — nothing else in this step's shape needs to change when it lands, since spend already proves the "live data through a fake server" pattern the RPM/TPM addition would reuse.
- **`auth.Registry` becomes mutable via copy-on-write, not a mutex-protected in-place edit.** `Update` never mutates an existing `*Team`; it builds a new value, validates it, and swaps the pointer into both the `byHash` and `byID` indexes under one write lock. A request that already holds the `*Team` `Authenticate` handed it keeps reading the old, now-orphaned struct for the rest of its lifetime — a PATCH mid-request cannot change the limits a request is already being evaluated against, and no request-side code needs to know that. This is the same "in-flight requests continue on the old state" guarantee Step 4.4's config hot reload will give the whole file, applied here one team at a time, and it is *why* `RateLimits`/`MonthlyBudgetMicros` were already being passed into `ratelimit`/`budget` on every call instead of cached once (a decision made back in Steps 3.2 and 3.4, before Update existed to need it) — the very next request that authenticates picks up a PATCH with nothing to invalidate.
- **Two indexes, one mutex, kept manually in sync.** `Update` writes both `r.byID[id]` and `r.byHash[updated.KeyHash]` to the same new pointer inside one critical section. Missing either write would leave one index resolving the old team forever — proven directly in `TestRegistryUpdateKeepsBothIndexesInSync`, not just asserted.
- **`provider.Registry` gained `Configs()`, not a new method on `Provider`.** The interface stayed exactly as small as Phase 1 insisted on; the registry already builds every instance from a `[]Config` at construction, so retaining a copy of that slice for admin introspection touches nothing adapter-shaped.
- **The admin JSON boundary is where API keys get redacted, not the registry.** `provider.Registry.Configs()` returns `Config` unredacted, same as it always has internally — redaction is a presentation concern. `admin.providerView` is built field by field rather than embedding `provider.Config`, so there is no `APIKey` field left in the struct to accidentally serialize; a future field added to `Config` cannot leak through this endpoint by omission the way it could if the struct were embedded.
- **`SpentUSD`/`BudgetUtilization` are `*float64`, not `float64`.** `nil` means "not read for this response" (a Redis failure, logged server-side) and is distinguishable from a genuine `0` on the wire — a team at 95% of its cap reported as `"spent_usd": 0.0` on a Redis hiccup would be actively misleading to an operator glancing at a dashboard; `null` is honest about not knowing.
- **A `Spent` failure for one team does not fail `GET /admin/teams` for every team.** The listing degrades per-row (that team's spend fields come back `null`, logged) rather than as a whole — an admin console going blank because one team's Redis read hiccuped is a worse failure mode than one row missing a column.
- **Budget fields in the wire format are USD decimals, converted at the boundary; PATCH accepts USD in, `Update` stores micro-dollars.** Matches the pattern `config.LoadTeams` already uses for `monthly_budget_usd` at boot — humans read and write dollars, the system accumulates micro-dollars, and the conversion happens exactly once, here, the same as it does there.
- **PATCH is scoped to "limits and budget" only — RPM, TPM, monthly budget — nothing else, matching the plan's own wording.** `auth.TeamPatch` has no field for allowlists, priority, or name. Widening it was tempting (it would have been nearly free given the plumbing already exists) but wasn't asked for, and CLAUDE.md's rule against adding anything beyond the current phase applies to a PATCH field exactly as much as to a whole endpoint.
- **Every PATCH and reset-budget call is logged with an actor, before values, and after values, per the plan's checklist — but "actor" is the caller's remote address, not an identity.** Part 1 has no admin authentication at all (CLAUDE.md scopes auth to static API keys on the public API only, and lists "auth beyond static API keys" as out of scope entirely). Logging a fabricated identity would be worse than logging what is actually known; the address is the honest ceiling on what this system can attest to right now.
- **`resetBudget` deletes the Redis key rather than setting it to `0`.** A team that has spent nothing this period and one whose spend was just reset are the same state, and `DEL` makes that literally true rather than merely numerically true — no key lingers with a synthetic zero that behaves subtly differently from "never touched" (e.g. under `Reserve`'s `is_new` TTL-setting branch, which a lingering zeroed key would skip).
- **Admin route paths keep an explicit `/admin` prefix even though the whole listener is already the admin port.** Redundant on the surface, but it leaves the root free for `/metrics` (Prometheus's own convention, arriving in Phase 9) and any future operator endpoint without a namespace collision — cheap to keep, expensive to retrofit.

### Step 4.4 — Config hot reload

- **`POST /admin/reload`, not an `fsnotify` watcher.** Presented as an explicit trade-off, not picked silently, since the plan offers both and one side adds a dependency. `fsnotify` replaces nothing in the standard library — Go has no cross-platform file-change-notification primitive, so the alternative is polling — and its cost here is a new dependency to justify, a background goroutine needing a context-tied lifecycle per CLAUDE.md's concurrency rule, and debounce handling for editors that fire multiple write/rename events per save (a specifically rough edge on this Windows dev machine, where `fsnotify`'s atomic-save handling is known to be less clean than on Linux). `POST /admin/reload` needs none of that: zero new dependencies, reuses `config.LoadProviders`/`LoadTeams` exactly as boot already calls them, and fits directly alongside the admin API Step 4.3 just built. Traded away: editing `configs/*.yaml` on disk does nothing until something calls the endpoint.
- **One `configStore`, not five wrapper types.** The atomic swap point implements `proxy.Resolver`, `proxy.Authenticator`, `proxy.CostCalculator`, `admin.TeamStore`, and `admin.ProviderLister` all at once, on a single type, by reading through one `atomic.Pointer[liveConfig]` on every method call. Go's structural typing is what makes this legal — five call sites each declare only the narrow interface they need, and one concrete type happens to satisfy all five — so the alternative (five near-identical structs each wrapping the same pointer) would have been pure duplication for no safety benefit. Compile-time assertions (`var _ proxy.Resolver = (*configStore)(nil)`, one per interface) catch any drift at the type definition itself instead of at a confusing `NewRouter` call-site error.
- **`liveConfig` bundles the provider registry, auth registry, and cost calculator into one struct, swapped by one `Store` call.** This is what makes a reload atomic *across files*, not just atomic per file: a request can never observe a new `providers.yaml` generation's registry paired with the previous generation's team registry or pricing table. Proven directly — `TestReloadRejectedTeamsLeavesBothFilesOnOldConfig` reloads a valid `providers.yaml` alongside a broken `teams.yaml` and confirms the provider side never swaps in either, even though it was individually valid.
- **Reload lives in `cmd/gateway`, in a new file, not a new `internal/` package.** Orchestrating across `config`, `provider`, `auth`, and `budget` to rebuild every registry is exactly `cmd/gateway`'s stated job — "entrypoint, wiring, graceful shutdown" — and it is already the one place allowed to import every package. A dedicated `internal/reload` package was considered and rejected: it would need to import `proxy` and `admin` to satisfy their consumer-defined interfaces, and nothing but `cmd/` is allowed to import `proxy` at all.
- **`loadLiveConfig` is one function called from two places — boot and reload — not two similar code paths.** Boot's `run()` and `newReloader`'s closure both call it, so "what counts as a valid config" can never quietly drift between "what starts the gateway" and "what a running gateway will accept as an update."
- **Validate fully before the only mutation.** `loadLiveConfig` returns an error from `config.LoadProviders`, `provider.NewRegistry`, `config.LoadTeams`, or `auth.NewRegistry` before `configStore.current` is ever touched — there is exactly one write in the whole reload path (`store.current.Store(next)`), and it only runs after every validation step already succeeded. This is what makes "a bad config must not take down a running gateway" true by construction rather than by a rollback step that could itself fail partway — proven in `TestReloadRejectedLeavesStoreOnOldConfig` and `TestReloadRejectedIs400`, one at the `configStore` level and one at the HTTP layer.
- **A reload silently discards any Step 4.3 `PATCH` made against the old registry.** Rebuilding `authRegistry` fresh from `teams.yaml` on every reload means an in-memory `PATCH` that was never written back to the file is superseded the moment someone reloads — the file is authoritative, the same way `kubectl apply` overwrites a live edit to a ConfigMap. Not treated as a bug: a full reload is explicitly "what's on disk, now," and a `PATCH` was always documented (Step 4.3) as a runtime override, never a promise of persistence.
- **A reload landing between two `Load()` calls within one request is an accepted, unaddressed race.** Each `configStore` method independently calls `current.Load()`, so a reload happening in the microsecond gap between (say) `resolve()`'s `ForModel` and `reserveTokens`'s `DefaultMaxTokensFor` could theoretically pair a model resolved against the old registry with a lookup against the new one. Closing this fully would mean pinning one snapshot per request — new plumbing to carry a `*liveConfig` through the request context — for a benefit out of proportion to how this reload actually runs: a rare, manual, operator-triggered `POST`, not a hot path. The failure mode if it ever fires is a single spurious error that succeeds on immediate retry, not a wrong answer or a security issue.
- **The old `*provider.Registry`/`*auth.Registry`/`*budget.Calculator` need no explicit shutdown after a swap.** Nothing they hold requires an explicit `Close` — HTTP clients release idle connections on their own — so the previous generation is simply garbage the next GC cycle collects once the last in-flight request holding it finishes. No goroutine, no drain logic, no lifecycle beyond normal Go memory management.
- **Verified live, not just under `go test`.** Ran the real gateway, `POST`ed a valid reload and confirmed `/admin/providers` still served correctly, then corrupted `configs/providers.yaml` on disk (`type: bogus-type`), confirmed the reload attempt came back 400 with the actual validation message, and confirmed `/healthz` and `/admin/providers` kept answering from the untouched old config throughout — the checklist's "invalid config reload is rejected, gateway keeps running" holds against the real process, not only the in-memory test.

---

## Phase 5 — Health Checking

- **Active pings alone react too slowly; passive signal is a live window fed by every real request.** Active check every ~30s/provider; passive samples every request, in-memory. Combining both catches a fast-degrading provider between scheduled pings.
- **Status is re-evaluated only on the active checker's tick, never on the request path.** Recording a passive sample is a cheap in-memory write; the threshold math and Redis write happen at check cadence (~30s/provider), not per-request — keeps this phase inside the project's <10ms overhead budget.
- **Two independent paths to Down: N consecutive failed pings, or passive error rate ≥ 50%.** Checked in that order, so the more severe reason wins when both are true at once.
- **Degrading is immediate; only the climb back to Healthy is hysteresis-gated.** One bad reading escalates status right away. Recovery needs N consecutive clean evaluations in a row — without this a provider hovering at a threshold flaps every check.
- **"Clean" for recovery means that tick's own ping succeeded, not just that the aggregate status read Healthy.** A lone ping failure below the down-threshold doesn't move the status by itself, but must still reset the recovery streak — caught by a failing test where a single bad ping mid-recovery wasn't resetting the counter.
- **p99 baseline is one in-memory EMA per provider (α≈0.2), not a second rolling window.** Rejected a literal second "baseline" ring buffer as unjustified extra bookkeeping. Not persisted to Redis — it's a local heuristic input, not the status itself, so replicas can each track their own without needing to agree.
- **Every Step 5.1/5.3 threshold (check interval/timeout, error-rate thresholds, consecutive-failure and recovery counts) is an env var, not a `configs/providers.yaml` field.** These describe how cautious the deployment wants to be, not per-provider business config — same category as `SWITCHYARD_DRAIN_TIMEOUT`.
- **Status is authoritative in-memory per replica; Redis is best-effort, for cross-replica visibility only.** A Redis read/write failure never blocks or changes this replica's own decision — matches CLAUDE.md's "health checker stale → treat provider as healthy."
- **Both the passive request window and the transition history are fixed-capacity ring buffers, not unbounded slices.** Long-running, per-provider, forever — an append-only slice would leak memory.
- **`/readyz` fails only when every provider is Down, never on a single bad one.** One dead provider is a routing problem for Phase 6's fallback chains, not a reason to pull the whole gateway from a load balancer.

---

## Phase 6 — Retry & Fallback Routing

- **Retry the same provider first, then fall back — and never return to a provider already given up on.** Expressed as loop nesting (retry inside the chain walk), not a flag, so the ordering can't drift.
- **Full jitter (`rand(0, base × 2^n)`), not fixed exponential backoff.** Fixed delays keep many clients that failed together synchronized, so they re-hit the recovering provider in the same instant. A provider's own `Retry-After` always wins over the computed delay — cooperating with a stated recovery time beats guessing.
- **Retryable and fallback-eligible are different questions, and the asymmetry is deliberate.** Retryable asks "will *this* provider answer differently in a moment"; fallback asks "whose fault is this failure." An upstream auth failure is *not* retryable (the same key gets rejected again) but *is* fallback-eligible (it's our credential, and a working provider is exactly the right answer). A malformed request or content-policy refusal is neither — it fails identically everywhere, so walking the chain is guaranteed-failing load and four round trips before the caller gets the same 400.
- **Fallback is defined per tier, not per model, and a model belongs to at most one tier.** "Requested model → its tier" needs an unambiguous answer; two tiers containing the same model has no defensible tiebreak, so it's a startup error.
- **Tier config is validated at load, and a tier entry naming a disabled provider is dropped, not rejected.** A chain pointing at a model nobody serves would surface as a mystery failure on the day the primary went down — the worst possible moment to discover it. Dropping (rather than rejecting) disabled entries keeps the intended chain documented while a paid slot is switched off.
- **Allowlist beats availability — the compliance point.** A team is never routed to a provider it isn't permitted to use, even when that provider is the only healthy option left; it gets an error instead. Applied uniformly to the requested model too, not just to fallbacks, so there is no path by which a request reaches a provider the team isn't allowed to use.
- **Health is a signal, not a verdict.** Down providers are skipped and degraded ones sink below healthy ones, but if *every* permitted candidate is down, the chain is tried anyway — a request that might succeed beats a request the gateway refused to attempt. The health relaxation never re-admits a forbidden provider; only authorization is absolute.
- **Total attempts are bounded across the whole chain (5), not per layer.** 3 retries × a 5-entry tier is 15 upstream calls hitting a fleet at its weakest moment. The primary spends its full retry allowance first, since a fallback is already evidence the first choice is unhealthy.
- **Chain exhaustion returns 503 with a per-candidate breakdown; a lone candidate keeps its own status.** With one option there is nothing to break down and the provider's own 429/504/502 says more than a flat 503. The breakdown ships as an extension field inside OpenAI's error envelope, so existing client error handling is untouched.
- **Streaming falls back only before the first byte.** Once a chunk is on the wire the status line is gone and a retry would duplicate content the client already has, so a mid-stream failure gets an SSE error event and ends there.
- **A timed-out primary doesn't poison the fallback's context.** Each adapter derives its own timeout from the request context internally, so the caller's context stays alive for the next candidate — the classic "fallback dies instantly on a cancelled context" trap is avoided by construction, not by re-plumbing a fresh context.
- **A costlier fallback reserves only the price *difference*.** The request already holds a reservation sized for the requested model; the top-up covers the gap, and the base reservation settles the real cost while top-ups settle to zero. Billing is always at the served model's price, never the requested one's.
- **An unaffordable fallback is skipped, not fatal — and exhausting affordable options returns 402, not 503.** The next entry down a tier is usually cheaper, so a denial is a reason to keep walking. A 503 would invite a retry against a cap that isn't going to move before the month ends.
- **Chain construction is a pure function** (candidates in, ordered chain out, no I/O), so the ordering rules are tested without a registry, a provider, or a Redis.
