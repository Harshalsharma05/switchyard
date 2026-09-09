# TIER 1 — Fault Tolerance & Persistent Team Storage

SwitchYard, Tier 1. Assumes `part2-complete` and the Settings screen from Phase 6 are done.

This is the first work aimed at SwitchYard being used by someone other than you. Three things: make the process hard to kill, move teams into a real store, and verify that the graceful degradation you've claimed actually holds.

`CLAUDE.md` still governs how work is done. `DESIGN.md` still governs anything visual. `PART1_PLAN.md` and `PART2_PLAN.md` remain the reference for what exists.

---

## Ground rules

1. **Nothing in Tier 1 adds a feature.** Every item here either removes a way the system can fail, or removes a lie in the documentation. If a step starts feeling like a feature, it belongs in Tier 2.

2. **Auth cannot fail open.** This is the one place `CLAUDE.md`'s "gateway must never be the reason a request fails" does not apply. Rate limiting failing open costs you nothing; authentication failing open means anyone can use the gateway. When the team store is unreachable, auth serves from cache or refuses — never allows.

3. **Overhead p95 < 10ms still holds.** Team lookup moves from an in-memory map to Postgres. If that lands on the hot path unguarded, it will blow the budget. Caching is a requirement of Phase 2, not an optimisation.

4. **Only teams move to Postgres.** `providers.yaml` stays a file — provider config is deployment configuration, not tenant data, and it has no runtime mutation path. Do not migrate it.

5. **One phase per session.** `/clear` between phases, run the checklist, commit, tag, append to `DECISIONS.md`.

---

## Phase 0 — Prerequisites

Nothing to install; Postgres is already in Compose from Part 2.

### 0.1 — Take stock of what reload currently breaks

Before changing anything, write down the current behaviour so you can prove you fixed it:

- Edit a team's RPM via the admin API, then `POST /admin/reload`. Confirm it reverts.
- Rotate a key, then reload. Confirm the old key works again.
- Check whether `demo.sh` calls `/admin/reload` anywhere. If it does, note where — the demo may be silently breaking already.

Record what you observe. This is your before-state.

### 0.2 — Inventory the panic surface

Find every goroutine the gateway starts that isn't a request handler. From memory of the build: the log writer's flusher, the health checker's ticker, the quality verification worker, the config file watcher, and anything the load simulator spawns. List them — Phase 1 needs to cover all of them, not just the request path.

### ✅ Phase 0 checklist

- [ ] Reload-reverts-changes behaviour observed and written down
- [ ] `demo.sh` checked for `/admin/reload` calls
- [ ] Complete list of long-lived goroutines written down

---

## Phase 1 — Panic Recovery

Small phase, high value. Go terminates the entire process on an unrecovered panic in *any* goroutine — so today a single malformed request, or one bad row from the quality worker, takes the whole gateway down.

### Step 1.1 — Request-path recovery

Middleware, outermost in the chain — it must wrap everything else, including auth, so a panic anywhere inside is caught.

On panic: recover, log at error level with the full stack trace and the request ID, increment a `switchyard_panics_total` counter labelled by route, and return a 500 with a generic body. **Never put the panic message or stack in the response** — it leaks internals to a caller.

The process keeps serving. That is the entire point.

### Step 1.2 — The streaming case 🧠

A panic mid-stream is the same problem as a mid-stream provider error from Phase 2 of Part 1: the 200 has already gone out and the status code can't be retracted.

Reuse the existing mechanism — emit the error event into the stream and close it. Do not invent a second pattern. If the existing stream-error path isn't reachable from the recovery middleware, say so rather than duplicating it.

### Step 1.3 — Background goroutine recovery

Every goroutine from your Phase 0.2 inventory needs its own recovery. A panic in the quality worker must not kill the gateway.

Write one helper — `safeGo(name string, fn func())` or similar — that wraps a function with recover, logs with the goroutine's name, increments the panic counter labelled by goroutine, and restarts the loop if the goroutine is meant to be long-lived. Use it everywhere. Do not hand-roll `defer recover()` in five places.

Decide 🧠: does a panicking background worker restart immediately, back off, or stay dead and mark itself unhealthy? A worker that panics in a tight loop and restarts instantly will spin. Back-off with a cap is probably right; record the choice.

### Step 1.4 — Prove it

Add a chaos endpoint action — `panic` — alongside the existing chaos controls, gated behind the same dev-only flag. Forces a panic in a request handler on demand.

This is how you demonstrate the recovery works, and it belongs with the other chaos controls in Live Ops rather than as a special case.

### ✅ Phase 1 checklist

- [ ] Trigger a panic via chaos: that request returns 500, the process stays up, subsequent requests succeed
- [ ] Panic during a streaming response: client gets a clean error event, connection closes, process stays up
- [ ] Stack trace appears in logs with the request ID; **nothing internal appears in the response body**
- [ ] `switchyard_panics_total` increments with the right label
- [ ] Force a panic in a background worker: the worker recovers or backs off, the gateway keeps serving
- [ ] Every goroutine from the Phase 0.2 inventory is covered — verified by reading, not assumed
- [ ] Panic recovery middleware is genuinely outermost — a panic in auth middleware is caught
- [ ] `go test -race ./...` clean
- [ ] `DECISIONS.md`: streaming panic handling, worker restart policy

**Tag: `tier1-panic-recovery`**

---

## Phase 2 — Postgres-Backed Team Storage

The substantial phase. It unblocks team creation, fixes the reload bug, and puts the schema in shape for organisations without building them.

### Step 2.1 — Schema 🧠

Two tables in one migration.

**`organizations`** — id, name, created_at. That is deliberately all. No budget, no limits, no settings. It exists so `teams.organization_id` can be a real foreign key today rather than a migration later.

**`teams`** — id, organization_id (FK), name, priority, rpm limit, tpm limit, monthly budget in integer micro-dollars, allowed providers, allowed models, is_admin, key_hash, key_source, key_masked, key_created_at, created_at, updated_at.

Decisions to make and record:

- **Allowlists**: a JSONB column, or a join table? JSONB is simpler and matches how they're used (read whole, replaced whole). A join table is more relational but buys nothing here. Pick and justify.
- **Budget stays integer micro-dollars**, same as Part 1's cost accounting. Do not introduce a float.
- **`organization_id` nullable or a default org?** A default "personal" org for every existing team is cleaner than nullable — it means no code ever handles a team with no org. Recommended, but decide.

Index on `key_hash` — that is the auth lookup and it is on the hot path.

**Never store a plaintext key.** Same rule as Part 2. Only the hash.

### Step 2.2 — One-time import from YAML

A migration or a small command that reads the existing `configs/teams.yaml` and inserts those teams into Postgres, creating a default organisation for them.

Idempotent — running it twice must not duplicate teams. It runs once, on the deploy where this ships.

After the import, `configs/teams.yaml` becomes dead. Decide 🧠: delete it, or keep it as a seed file for fresh installs? Keeping it means having a clear rule about which wins — and "config file plus database, both authoritative" is exactly the confusion this phase exists to remove. Recommendation: keep it strictly as a first-boot seed for an empty database, never read again once teams exist. Document the rule wherever it lives.

### Step 2.3 — The store, and the cache that makes it viable 🧠

Auth happens on every single request. A Postgres round-trip per request is not acceptable against a 10ms overhead budget.

Design: an in-memory cache of teams keyed by key-hash, backed by Postgres, sitting behind the same `auth.Registry` interface the rest of the code already uses. The proxy's auth path should not know the store changed.

The hard part is invalidation, and it is a genuine design decision:

- **Short TTL** (30–60s) — simple, works across replicas with no coordination, but a revoked key stays valid for up to the TTL. That is a real security window and must be stated, not glossed.
- **Redis pub/sub on change** — near-instant invalidation across replicas, more moving parts, and a new failure mode if Redis is down (fall back to TTL).
- **Write-through on the local instance plus TTL for others** — a change is instant on the replica that made it, TTL-bounded elsewhere.

Pick one, write down the trade-off honestly, and state the revocation window in the docs. There is no free answer here.

**Postgres unreachable:** serve from cache, even past TTL, and mark the system degraded. A key already in cache keeps working; a key not in cache is refused. This is the one place you serve stale data deliberately — the alternative is a total outage every time the database hiccups. Log loudly, fire a metric, surface it in the System panel.

**Never fail open.** An unknown key is rejected whether the store is healthy or not.

### Step 2.4 — Fix the reload bug

With teams in Postgres, `POST /admin/reload` re-reads `providers.yaml` only. It must no longer touch teams, and therefore can no longer revert a key rotation or a limit edit.

Verify against your Phase 0.1 before-state: the exact sequence that used to revert now doesn't.

Update the reload warning copy in the Settings System panel — it currently says reload reverts in-memory key rotation, which will no longer be true. Leaving stale warning text is its own kind of bug.

Remove the "documented limitation" note from Part 2 Phase 6 in `DECISIONS.md` — replace it with the resolution, don't delete the history.

### Step 2.5 — Team CRUD, finally

Now that there's a real store:

- `POST /admin/teams` — create, generate key server-side, return plaintext exactly once
- `DELETE /admin/teams/{id}` — delete. Decide 🧠: hard delete, or soft delete with `deleted_at`? Request logs reference team IDs; a hard delete orphans them. Soft delete is probably right.
- Existing PATCH, budget reset, key rotate, key revoke — now persist

Rotation and revocation must now survive a restart. That is the headline fix of this phase.

Every mutation still writes to the audit log, with the same fail-closed ordering from Part 2.

### Step 2.6 — Settings UI: create team

Replace the placeholder from Part 2 Phase 6 with a real form: name, organisation (single default for now, but present as a field so the concept exists), priority, RPM, TPM, monthly budget, provider and model allowlist, admin flag.

On success, the show-once key panel that already exists from Part 2 — reuse it, do not build a second one.

Delete flow with inline confirmation, per `DESIGN.md`.

The organisation field being present-but-single is deliberate: it makes the model visible without building multi-org behaviour.

### ✅ Phase 2 checklist

- [ ] Migration applies cleanly to an empty database and is idempotent
- [ ] YAML import runs once, creates the default org, and does not duplicate on re-run
- [ ] Every team from `teams.yaml` exists in Postgres with identical limits, budget, and allowlists
- [ ] **Overhead p95 still under 10ms** with the team store on the auth path — measured under load, not eyeballed
- [ ] Cache hit rate on team lookup is high enough that Postgres isn't queried per request — verified with a metric
- [ ] Create a team in the UI, use its key immediately, **restart the gateway, key still works**
- [ ] Rotate a key, **restart**, old key still 401s and new key still works
- [ ] Edit a limit, `POST /admin/reload`, **change survives** — the Phase 0.1 before-state is fixed
- [ ] Revoke a key: it stops working within the documented invalidation window
- [ ] Stop Postgres: cached keys keep working, uncached keys are refused, system shows degraded, **no request 500s**
- [ ] Restart Postgres: normal operation resumes with no gateway restart
- [ ] An unknown key is rejected whether Postgres is up or down — auth never fails open
- [ ] Deleted team's request-log rows are not orphaned
- [ ] Reload warning copy in Settings updated to match new behaviour
- [ ] `go test -race ./...` clean; coverage above 80% on the new store package
- [ ] `DECISIONS.md`: schema choices, allowlist storage, cache invalidation strategy with the revocation window stated, YAML seed rule, delete semantics

**Tag: `tier1-postgres-teams`**

---

## Phase 3 — Graceful Degradation Audit

You have claimed fail-open behaviour for Redis, Postgres, and telemetry across three plan documents. Some of it has been tested individually, in passing. This phase verifies all of it systematically and writes down what is actually true.

This is not a feature phase. The deliverable is a document and whatever fixes the testing forces.

### Step 3.1 — The matrix

Build a table. For each dependency, state the intended behaviour, then test it and record the actual behaviour:

| Dependency | Intended | Actual |
|---|---|---|
| Redis down | Rate limit fails open, budget fails closed, breaker uses local cache, health degrades | ? |
| Postgres down | Request logging drops rows, team auth serves from cache | ? |
| Jaeger / OTLP down | No effect on requests | ? |
| Prometheus down | No effect on requests; dashboards degrade | ? |
| Embedding source down | Cache disabled, requests proceed | ? |
| All providers down | 503 with per-candidate breakdown | ? |
| Quality worker backed up | No effect on requests | ? |

Test each one by actually stopping the service and sending traffic. Not by reading the code.

### Step 3.2 — Combinations

Single failures are the easy case. Test the ones that plausibly co-occur:

- Redis **and** Postgres down together — this is the realistic "shared infra outage" case. Does auth still work from cache while rate limiting fails open? Does anything deadlock?
- Postgres down **during** a key rotation
- Redis down **during** an active circuit breaker cooldown
- Provider down **and** Redis down — does fallback still work when breaker state can't be shared?

You are looking for interactions, not repeats of Step 3.1.

### Step 3.3 — Fix what's wrong, document what's acceptable

Some findings will be bugs — fix them. Some will be acceptable degradation that simply wasn't written down — document them.

The unacceptable outcome is a gap between what the README claims and what the system does. Either the behaviour changes or the claim does.

### Step 3.4 — Write it up

A `docs/failure-modes.md` with the completed matrix, the combination findings, and for each: what a user experiences, what an operator sees, and what recovers automatically versus what needs intervention.

This document is the answer to the single-point-of-failure question, and it is more convincing than any verbal answer because it is evidence rather than intention.

Add a short summary to the README linking to it.

### ✅ Phase 3 checklist

- [ ] Every dependency in the Step 3.1 matrix tested by actually stopping it
- [ ] Actual behaviour recorded, including where it differs from intent
- [ ] All four combination scenarios tested
- [ ] Every discrepancy either fixed or documented — none left as a known-unknown
- [ ] No dependency failure causes a 500 on a request that should have succeeded
- [ ] Auth never fails open under any combination
- [ ] `docs/failure-modes.md` complete, README links to it
- [ ] README and `DECISIONS.md` claims now match measured reality
- [ ] `DECISIONS.md`: anything the audit changed

**Tag: `tier1-complete`**

---

## Exit interview

Same rules — out loud, from memory.

1. A request panics. Walk through exactly what happens, from the panic to what the caller receives.
2. Why is the panic recovery middleware outermost rather than inside auth?
3. Your quality worker panics in a loop. What stops it spinning?
4. Team auth is now a database lookup on every request. How did you keep it under 10ms?
5. You revoke a key. How long can it still authenticate, and why is that non-zero?
6. Postgres is down and a request arrives with a valid key that isn't cached. What happens, and why is that the right call?
7. Why does auth fail closed when everything else in this system fails open?
8. `teams.yaml` still exists. What reads it, and when?
9. Redis and Postgres are both down. Which features still work?
10. Someone says your gateway is a single point of failure. What's your answer, and what in the repo backs it up?
