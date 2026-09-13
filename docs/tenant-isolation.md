# Tenant isolation

Multi-user Phase 4's audit. Every row below was produced by standing up two
real organisations — separate teams, separate sessions, separate request
history, cache entries, and audit trails — against the compiled `cmd/gateway`
binary over real Postgres and Redis, and attacking org A's session with org
B's IDs over real HTTP. Nothing here was confirmed by reading the code and
asserting what it should do; see [Reproduce](#reproduce) to redo any of it.

The two-organisation fixture, and every attack below, is a permanent
integration test — not a one-time manual pass. See
[test/tenant_isolation_test.go](../test/tenant_isolation_test.go),
[test/tenant_isolation_attack_test.go](../test/tenant_isolation_attack_test.go),
[test/tenant_isolation_cache_test.go](../test/tenant_isolation_cache_test.go),
and
[test/tenant_isolation_subtle_test.go](../test/tenant_isolation_subtle_test.go).
A future change that reopens any hole below fails `go test -tags integration
./test/...`, not a human's memory of this document.

## Summary matrix

| Vector | Expected | Actual |
|---|---|---|
| Cross-org reads (team detail, request detail) by path ID | 404, never 403 | Confirmed |
| Cross-org mutations (patch, delete, reset-budget, key rotate, key revoke) | 404, never 403 | Confirmed |
| Naming another org on team creation | 400, nothing created | Confirmed |
| `?team=` naming another org's team on the six cross-team read endpoints | Silently falls back to the caller's own scope, never matches | Confirmed |
| Paging a request-history list past its own end | Empty, never spills into the other org | Confirmed |
| Semantic cache: identical prompt, both org orderings | Neither org ever receives the other's cached answer | Confirmed |
| Cost and quality aggregates | Exactly the caller's own seeded numbers, never inflated | Confirmed |
| Error bodies from cross-org attempts | Never name the other org's real ID or display name | Confirmed |
| JWT with a tampered `org` claim | Rejected outright (401) by signature verification | Confirmed |
| Revoked refresh token | Cannot mint a new access token | Confirmed — unit-level, see [Residual risks](#residual-risks) |
| Team API key against the admin port (`:9090`) | Rejected — not read as a credential at all | Confirmed |
| Session cookie against the gateway port (`:8080`) | Rejected — not read as a credential at all | Confirmed |
| Operator (superadmin) action on org A's own resource | Visible to org A, actor shown as `platform-operator`, address blank | Confirmed |
| Operator action on org B's resource, read as org A | Invisible to org A | Confirmed |
| No-tenant audit entries (config reload) | Visible only to the superadmin, with the real actor | Confirmed |

## Method

Two organisations, each with two teams: org A reuses `teamstore.DefaultOrgID`
(`"personal"`), seeded the way production seeds a first-boot database
(`configs/teams.yaml` via `cmd/migrate`); org B is a second organisation
created the only way a second org legitimately can be — a real `organizations`
row followed by its teams created over the genuine `POST /admin/teams`, the
same endpoint a real org-B signup would use. Both orgs carry request history
(one quality-scored row each), audit history (an org-A self-action, an org-B
self-action, a superadmin action against one of org A's own teams, and a
no-tenant config reload), and — in a second, purpose-built fixture where both
orgs share one provider and model — cache entries for the identical prompt.

Every attack is a real HTTP call against the compiled binary's admin port,
authenticated with a session cookie signed the same way the harness signs a
superadmin's for every other integration test in this repo (`sessionFor` in
`test/harness_test.go`). No fake stores, no direct package calls standing in
for the network boundary that actually matters here.

## What was tested

### Every endpoint, attacked directly

For each of `GET /admin/teams/{id}`, `GET /admin/requests/{id}`, `PATCH
/admin/teams/{id}`, `DELETE /admin/teams/{id}`, `POST
/admin/teams/{id}/reset-budget`, `POST /admin/teams/{id}/key/rotate`, and
`DELETE /admin/teams/{id}/key`: org A's session was given org B's team or
request ID. Every one answered 404 `team_not_found` or `not_found` — never
403, which would have confirmed the ID was real. The same calls against org
A's own resources, run in the same test as a control, succeeded normally, so
the 404 is provably about ownership and not a broken route.

The one case that is not a 404 by design: naming another organisation on
`POST /admin/teams`. There is no existing team to look up, so `createOrgFor`
(`internal/admin/teams.go`) rejects it with 400 before anything is created —
confirmed nothing appeared in org B's own subsequent team list.

### Query-param fallback, not a leak

`/admin/requests`, `/admin/costs`, `/admin/attribution`, `/admin/reconciliation`,
`/admin/quality/feedback`, and `/admin/summary` all accept `?team=` to narrow
within the caller's own scope (`orgScope` in `internal/admin/gate.go`). Naming
org B's team from org A's session does not error — the correct behaviour is
documented as "fall back to the caller's full own-org scope" — so the test
asserts the stronger claim: org B's team ID never appears anywhere in the
response body, on all six endpoints.

### Pagination

Org A's own three-row request history was paged one row at a time to its
end. Every page's rows belonged only to org A's own teams, the terminal page
came back with no `next_cursor`, and the running total matched exactly 3 —
proving there is nothing to walk into past the end, not merely that no error
was thrown there.

### The semantic cache (Step 4.3's headline case)

This is the one vector where a request never mentions another organisation's
ID anywhere, so the only way to catch a leak is to send the identical prompt
from both orgs and watch which one actually reaches the provider. Two teams
sharing one provider and model sent a byte-identical prompt in both orderings
(A then B, and B then A), read through the real `X-Switchyard-Cache` response
header and the mock provider's own hit count — not by inspecting Redis
directly. In both orderings, the second org's request was a genuine miss
(`X-Switchyard-Cache: miss`, one new provider call), never `exact`. A same-org
repeat, run as a control, correctly came back `exact` with zero new provider
calls, confirming the harness can produce a hit at all and this is a real
negative, not a broken cache.

Cache scope is keyed by `team.OrganizationID` resolved server-side from the
authenticated team
([internal/proxy/cache.go](../internal/proxy/cache.go)), never from anything
the client supplies — the same "scope comes from the verified identity, never
the request" rule the rest of Phase 4 rests on.

### Aggregates

Cost and quality-feedback totals were checked against the fixture's known
seed values, not just "no error" — each org's three rows cost exactly 3000
cost-micros in total, and each org's downgraded-quality count is exactly 1,
never 2. This is deliberately a different assertion from the query-param
check above: an aggregate can be wrong by a number without ever naming the
other org anywhere in the response, which a substring check cannot catch.

### Errors

A selection of org A's cross-org 404s (team detail, reset-budget, an
unknown request ID) were checked for org B's real display name and real
organisation ID — not the team ID, which the attacker supplied itself and
therefore already "knows," but the org's name and ID, which it has no
legitimate way to know. None appeared in any response body.

### Token tampering

A validly-signed org-A session cookie had its `org` claim rewritten to org
B's ID, keeping the *original* signature — exactly what an attacker without
the signing secret would produce by editing the JWT's (signed, not
encrypted) payload. The gateway rejected it with 401 before org scoping ever
had a chance to run, which is the point of signing the claim in the first
place: a forged claim never reaches the authorization logic at all.

### Cross-identity, re-verified

A team API key sent as a Bearer token to the admin port, and a session
cookie sent to the gateway port, were both rejected. This mirrors Phase 1's
own unit tests (`internal/proxy/auth_test.go`,
`internal/identity/middleware.go`) but was re-run here against the real
compiled binaries on both real ports, per the plan's explicit instruction not
to trust the unit-level result alone.

### Audit visibility, both halves

Confirmed in one direction already covered by Phase 2's design and
re-verified here: org A sees the superadmin's action against org A's own
team, with the actor shown as the `platform-operator` sentinel and the actor
address blank — never the operator's real user ID or IP. Confirmed in the
other direction: org A never sees an entry targeting org B's team, and
neither org sees the no-tenant config-reload entry, which is visible only to
the superadmin's unscoped read, shown there with the real actor identity
(unscoped reads never redact).

## Residual risks

Stated plainly, as the plan requires — these are gaps, not this document
overclaiming coverage it doesn't have.

- **Cache purge by team** (`DELETE /admin/cache?team=`) was not attacked at
  the integration level. Its org-check — a foreign team is 404, not 403 — is
  covered by `TestPurgeCacheAuthorization` in `internal/admin/teams_test.go`
  at the unit (fake-store) level, but this repo's integration harness does
  not yet stand up a cache-enabled gateway and attack this specific route
  end to end.
- **Revoked refresh tokens** are unit-tested only
  (`TestRotateRejectsUnknownExpiredAndRevoked` in
  `internal/identity/session_test.go`), not integration-tested. Building a
  real session through this repo's integration harness would require either
  a live Google OAuth round trip (this harness has none) or a password-based
  signup flow, which is not implemented — Phase 1 is Google-only.
- **The semantic cache tier** (as opposed to the exact tier exercised above)
  was not empirically verified for cross-org isolation. It shares the same
  `Scope`-derived key construction, so the same guarantee should hold
  architecturally, but the semantic path needs a real embedder and API key
  this test harness deliberately avoids depending on.
- **A genuine unhandled error (500)** leaking another org's data was not
  forced. Every error path exercised here was a deliberate, correctly-typed
  404 or 400; whether an unexpected panic or database error could ever
  surface another tenant's data in a stack trace or generic error body was
  not tested, because it isn't reliably forceable without fault injection
  this phase doesn't build.
- **Timing side channels** — whether a 404 for "no such team" takes
  measurably different time than "not yours" in a way that would let an
  attacker distinguish them — were not measured.
- **True concurrency** was not tested for the cache: both orderings were
  sequential, deliberately synchronized (`warmCache` in
  `test/tenant_isolation_cache_test.go`) so the assertions are deterministic.
  Two organisations racing to write the same fingerprint bucket at the exact
  same instant was not exercised.

None of the above were treated as blocking — each is a boundary this audit
did not reach, not a known failure inside a boundary it did reach.

## What the checklist confirms

- Every cross-org attempt in Step 4.2's inventory — reads, mutations,
  pagination — returned 404, never 403, and never mutated the other org's
  resource.
- Both subtler vectors the plan calls out as most warranting a permanent
  test — cache isolation and cross-org mutation — are automated integration
  tests today, not one-time manual checks.
- Two real bugs surfaced during this audit, both in the test fixture itself
  (a Postgres argument-count mismatch, and an undercounted audit-row
  expectation once org B's teams turned out to be created over the audited
  admin API rather than seeded like org A's) — not in the product. Both are
  fixed; see the git history on the four test files above.
- `go test -tags integration -race ./test/...` and `go test -race ./...`
  both ran clean against real Postgres and Redis, with one unrelated,
  pre-existing flake found and confirmed independent of this work
  (`TestListTeamsScopedToCallersOrg` in `internal/admin/teams_test.go` — an
  ordering assertion on a superadmin's cross-org team list that doesn't sort
  before comparing; reproduces identically on the pre-Phase-4 tree).

## Reproduce

PowerShell, from the repo root, with Postgres and Redis up
(`docker compose -f deploy/docker-compose.yml up -d postgres redis` from
`deploy/`) and `POSTGRES_PASSWORD` set to match `.env`:

```powershell
$env:POSTGRES_PASSWORD = "<value from .env>"
go test -tags integration -race ./test/... -run 'TestTenantIsolationFixtureSetup|TestCrossOrgAccessReturns404|TestCreateTeamInAnotherOrgIsRejected|TestReadEndpointsStayWithinCallersOrg|TestAuditListNeverLeaksAnotherOrgOrOperatorIdentity|TestCacheIsolationAcrossOrgs|TestAggregatesStayWithinCallersOrg|TestCrossOrgErrorsDoNotNameTheOtherOrg|TestTamperedJWTOrgClaimIsRejected|TestCrossIdentityRejectedOnBothPorts|TestNoTenantAuditEntriesAreSuperadminOnly' -v
```

Every test spins up its own compiled `cmd/gateway`, a throwaway Postgres
database, and (for the cache test) its own cache configuration — nothing
here depends on state left over from a previous run.
