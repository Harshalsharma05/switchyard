# MULTI-USER — Implementation Plan

SwitchYard multi-tenancy. Assumes `tier1-complete` is tagged: panic recovery in place, teams in Postgres with an `organizations` table and a default org, failure modes documented.

This is the work that turns SwitchYard from a system you run into a product other people can use. `CLAUDE.md` governs how work is done. `DESIGN.md` governs anything visual. Prior plans remain the reference for what exists.

---

## Ground rules

1. **Two identity types, never interchangeable.**
   - **Team API keys** — machines. Claude Code, SDKs, scripts. Public port `:8080`. Unchanged by this work.
   - **User sessions (JWT in a cookie)** — humans. Dashboard only. Admin port `:9090`.

   A team key must never authenticate a dashboard request. A session must never authorise a gateway completion. These are separate middleware, separate code paths, and a test proves each rejects the other.

2. **Org scope comes from the token, never from the request.** Every org-scoped query derives its org from the verified JWT claim. If any handler reads an org or team ID from a query parameter, path segment, or body and trusts it, that is a tenant-isolation bug regardless of what the UI sends.

3. **The gateway hot path does not learn about users.** This is a control-plane change. `:8080` continues to authenticate team keys exactly as it does today. Overhead p95 < 10ms is unaffected and must stay that way.

4. **The semantic cache fix is a security fix, not a feature.** Today the cache key omits tenant, so one org can receive a response generated from another org's request. That is a cross-tenant data leak the moment a second user exists. It ships in Phase 2 and is non-negotiable.

5. **Never store a password in any recoverable form**, and never log a token, a cookie value, an OAuth code, or a client secret. Same discipline as API keys in Part 2.

6. **One phase per session.** `/clear` between phases, run the checklist, commit, tag, append to `DECISIONS.md`.

---

## Repo additions

```
internal/identity/          users, orgs, sessions, password hashing
internal/oauth/             Google OAuth flow
internal/admin/middleware/  session auth, CSRF, org-scope extraction
migrations/                 users, sessions, org backfill
web/src/pages/auth/         login, signup, OAuth callback
web/src/hooks/useSession.js replaces the pasted-key hook
```

`internal/identity/` must not be importable from `internal/proxy/` or any provider or resilience package. If it is, the two identity systems have leaked into each other.

---

## Phase 0 — Prerequisites (manual)

### 0.1 — Google Cloud OAuth

- Create a project in Google Cloud Console, configure the OAuth consent screen (External, test users are fine for now).
- Create an OAuth 2.0 Client ID, type Web application.
- Authorised redirect URIs: your local callback (`http://localhost:9090/auth/google/callback` or wherever it lands) and a placeholder for production — you will add the real one at deploy time.
- Record client ID and secret in `.env`, same pattern as provider keys. **Never in YAML, never committed.**

### 0.2 — JWT signing secret

Generate a strong random secret into `.env`. Note in `DECISIONS.md` that rotating it invalidates every session, and that production will need a rotation story eventually — not now.

### 0.3 — Decide token lifetimes 🧠

Access token TTL and refresh strategy. Short access tokens with refresh is standard; long-lived single tokens are simpler but a stolen cookie stays valid longer. Pick, and state the compromise window explicitly — the same discipline as the key-revocation window from Tier 1.

### ✅ Phase 0 checklist

- [ ] Google OAuth client created, both redirect URIs registered
- [ ] Client ID and secret in `.env`, gitignored
- [ ] JWT secret generated and stored the same way
- [ ] Token lifetime and refresh strategy decided and recorded

---

## Phase 1 — Users, Organizations, and Authentication

### Step 1.1 — Schema

**`users`** — id, organization_id (FK), email (unique, citext or lowercased), name, avatar URL, auth provider (`google` | `password`), password hash (nullable — null for Google users), is_superadmin, created_at, last_login_at.

**`organizations`** — already exists from Tier 1. Add `owner_user_id` and a display name if not present.

**`sessions`** — if using server-side refresh tokens: id, user_id, refresh token hash, expires_at, created_at, revoked_at, user agent, IP. Never the raw token.

Decisions to record 🧠:

- **One org per user, created at signup.** No invites, no multiple members. Multi-member orgs are a later feature and building them now is speculative.
- **Email uniqueness across providers.** If someone signs up with Google and later tries email+password with the same address, what happens? Recommended: link to the existing user rather than creating a duplicate, but require the Google login to prove ownership first. Decide and write it down — this is a real account-takeover surface if handled carelessly.

### Step 1.2 — Google OAuth flow

Standard authorization code flow with PKCE. Endpoints on the admin port:

- `GET /auth/google` — generate state and PKCE verifier, store server-side or in a short-lived signed cookie, redirect to Google
- `GET /auth/google/callback` — verify state, exchange code, fetch profile, find-or-create user and org, issue session

**Verify `state` on every callback.** Without it this is a CSRF hole. Reject a mismatched or missing state outright.

On first login: create the user *and* their organization in one transaction. A user without an org is a broken state that nothing downstream should have to handle.

### Step 1.3 — Email + password fallback

- `POST /auth/signup` — email, password, name. Argon2id (or bcrypt with a sensible cost). Never anything hand-rolled.
- `POST /auth/login` — verify, issue session.
- Password rules: a minimum length, and nothing else. Composition rules push people toward worse passwords.

Timing: a login attempt for a nonexistent email must take the same time as one with a wrong password, and return the same error. Otherwise the endpoint is an email enumeration oracle.

No password reset flow this phase — note it as a gap. It needs email delivery, which is its own dependency.

### Step 1.4 — JWT in an httpOnly cookie 🧠

Access token as a JWT in a cookie with `HttpOnly`, `Secure` (in production), `SameSite=Lax`, scoped path, and an explicit expiry.

Claims: user ID, org ID, superadmin flag, expiry. Nothing sensitive — a JWT is signed, not encrypted, and anyone holding it can read the payload.

**Cookies mean CSRF.** `SameSite=Lax` covers a lot but is not sufficient alone for state-changing requests. Add either a double-submit CSRF token or an origin check on every mutating admin endpoint. Pick one and apply it uniformly — a partially-protected mutation surface is not protected.

`POST /auth/logout` clears the cookie and revokes the refresh token server-side. A logout that only clears the client cookie is not a logout.

### Step 1.5 — Session middleware, and keeping the two identities apart

New middleware on the admin port: read cookie, verify signature and expiry, load user, attach user and org to the request context.

The critical part:

- Admin-port endpoints accept **only** a session. A valid team API key presented to `:9090` is rejected.
- Gateway endpoints on `:8080` accept **only** a team key. A valid session cookie is ignored.

Write the test for both directions in this phase, not in Phase 4. This is the boundary the whole model rests on.

### Step 1.6 — Backfill existing data

Tier 1 created a default organization for the imported YAML teams. That org now needs an owner.

A migration or one-time command that creates your superadmin user and attaches it to that existing org, so nothing is orphaned. Idempotent.

### Step 1.7 — Frontend auth

Replace the pasted-key flow entirely. Login page with a Google button and an email/password form. OAuth callback handling. `useSession` hook replacing the key hook. Unauthenticated users are redirected to login; the post-login redirect returns them where they were going.

Because the JWT is httpOnly, the frontend cannot read it — session state comes from a `GET /auth/me` call on load, not from decoding a token client-side. Design for that.

### ✅ Phase 1 checklist

- [ ] Google signup creates user and org in one transaction; second login reuses both
- [ ] Email+password signup and login work; passwords are argon2id and never logged
- [ ] Nonexistent email and wrong password are indistinguishable in timing and response
- [ ] JWT is httpOnly — confirmed unreadable from `document.cookie` in devtools
- [ ] CSRF protection on every mutating admin endpoint, verified by a forged cross-origin request
- [ ] OAuth callback with a tampered or missing `state` is rejected
- [ ] **A team API key presented to `:9090` is rejected**
- [ ] **A session cookie presented to `:8080` is ignored; the request needs a team key**
- [ ] Logout clears the cookie and revokes server-side; the old token no longer works
- [ ] Expired token is rejected; refresh works per the chosen strategy
- [ ] Backfill attaches the existing default org to a superadmin user, idempotently
- [ ] No token, cookie value, OAuth code, or password appears in any log or audit row
- [ ] `go test -race ./...` clean; coverage above 80% on `internal/identity`
- [ ] `DECISIONS.md`: token lifetime, cookie vs header, CSRF approach, email-collision policy, password-reset gap

**Tag: `multiuser-auth`**

---

## Phase 2 — Org Scoping

The largest phase, and mostly editing existing code rather than writing new code. Every query that returns tenant data gains an org filter.

### Step 2.1 — Inventory first

Before changing anything, list every admin endpoint that returns or mutates tenant data: teams, request logs, cost summaries, quality scores, audit log, cache stats, budget state.

Work from the list. An endpoint missed here is a tenant leak that Phase 4 has to catch, and Phase 4 catching it is much later than catching it now.

### Step 2.2 — Scope the queries

Every one gains `WHERE organization_id = $1`, sourced from the JWT claim.

Watch for the indirect ones: request logs are keyed by team, not org, so scoping means "teams belonging to my org." Same for cost aggregates and quality scores. These are the joins most likely to be forgotten.

Add the index the new filter needs — `organization_id` on teams, and whatever the request-log join requires. Measure before adding more than that.

### Step 2.3 — Semantic cache isolation 🧠

**This is a security fix.** Today's cache key omits tenant entirely.

Add `org_id` to the cache key — prefix (`cache:org:{id}:…`) or metadata filter on the vector search, whichever fits your Phase 7 implementation more naturally.

Org, not team: one person's projects sharing a cache is useful and safe; two people sharing one is a leak.

Free-tier Redis considerations, since this multiplies entries:

- Per-org entry cap so one heavy user can't consume the whole cache
- `maxmemory-policy allkeys-lru` so Redis evicts rather than erroring when full
- Existing TTL from Phase 7.4 still applies

Expect the hit rate to drop. That is correct and should be stated in `DECISIONS.md`: *cache is org-scoped, accepting a lower hit rate, because a cross-tenant cache hit is a data leak.*

**Purge the existing cache on deploy.** Entries written before this change have no org and cannot be safely attributed.

### Step 2.4 — Rework the admin model

`is_admin` currently means "admin of everything." Split it:

- **Org admin** — manages teams, keys, budgets within their own org. This is what every normal user is.
- **Superadmin** — you. Cross-org visibility, org management. A flag on `users`, not on teams.

Every existing admin check needs review: is this "admin of my org" or "superadmin"? Most are the former. Getting one wrong in the superadmin direction is a cross-tenant hole.

### Step 2.5 — Audit log scoping

The audit log becomes org-scoped like everything else. Actor changes from team ID to user ID — you now have a real human identity to attribute changes to, which is what an audit log is for.

Superadmin actions are recorded and visible to superadmin only.

### ✅ Phase 2 checklist

- [ ] Every endpoint from the Step 2.1 inventory is scoped — checked off individually, not assumed
- [ ] Org scope comes from the JWT claim in every case; no handler trusts a client-supplied org or team ID
- [ ] Create two orgs with data in each; confirm neither sees the other's teams, logs, costs, quality scores, or audit entries
- [ ] **Org A cannot receive a cache hit generated by org B** — tested with deliberately identical prompts
- [ ] Pre-existing cache entries purged on deploy
- [ ] Per-org cache cap and LRU eviction configured and verified under a fill test
- [ ] Org admin can manage only their own teams; attempting another org's team returns 404, not 403 (do not confirm existence)
- [ ] Superadmin can see across orgs; a normal user cannot, even with a crafted request
- [ ] Audit entries attribute a user, are org-scoped, and superadmin actions are visible only to superadmin
- [ ] **Overhead p95 still under 10ms** — the gateway path should be untouched; confirm it
- [ ] `go test -race ./...` clean
- [ ] `DECISIONS.md`: cache scoping rationale and hit-rate trade-off, org-admin vs superadmin split, 404-over-403 choice

**Tag: `multiuser-scoped`**

---

## Phase 3 — Cross-Project Roll-Up

Now the payoff: one user, several projects, one view.

### Step 3.1 — Language

Teams become **projects** in the UI. Same entity, same table, clearer word for a solo user with three side projects. Change the labels, not the schema — a rename migration buys nothing and breaks every existing reference.

### Step 3.2 — The roll-up

A view (its own page, or the top of Overview — `DESIGN.md` decides placement) showing, for the signed-in org:

- Total spend this period across all projects
- Spend by project
- Spend by model, aggregated across projects
- Requests, tokens, cache hit rate, error rate — same aggregation
- Per-project budget status side by side

All of it is aggregation over rows you already have, grouped by org instead of filtered to one team.

### Step 3.3 — Project switching

A project selector in the top bar. "All projects" is the default; selecting one scopes every screen — Overview, Request Logs, Usage & Cost — to that project.

Selection lives in the URL so a filtered view is shareable and survives refresh, consistent with Phase 5's filter handling in Part 2.

### Step 3.4 — Settings, rescoped

Existing Settings mostly works. Now: shows only your org's projects, create/delete operates within your org, and the org field from Tier 1 Step 2.6 becomes real rather than a single default.

Optional 🧠: an org-level spend cap on top of per-project budgets. Genuinely useful ("no more than $50/month total however it's split") but it interacts with per-project enforcement in ways that need thought. Defer unless you want it now — per-project caps cover most of the need.

### ✅ Phase 3 checklist

- [ ] Roll-up totals match the sum of per-project figures exactly
- [ ] Roll-up matches an independent manual aggregation over the request log
- [ ] Project selector scopes every screen correctly; "all projects" is the default
- [ ] Selection persists in the URL and survives refresh
- [ ] A user with one project sees a sensible view, not an empty roll-up
- [ ] A brand-new user with zero projects sees a real empty state with a clear next action
- [ ] Creating a project from Settings works and appears immediately in the selector
- [ ] All copy says "project" consistently; no stray "team" in the UI
- [ ] `DECISIONS.md`: naming choice, roll-up placement, org-cap decision either way

**Tag: `multiuser-rollup`**

---

## Phase 4 — Tenant Isolation Audit

Same spirit as Tier 1 Phase 3, aimed at a different failure class. You are not testing that features work — you are actively trying to break isolation.

### Step 4.1 — Set up

Two orgs, each with several projects, request history, cache entries, quality scores, and audit entries. Plus your superadmin.

### Step 4.2 — Attack every endpoint

For each admin endpoint, signed in as org A, attempt to reach org B's data:

- Pass org B's team ID in a path parameter
- Pass org B's org ID in a query parameter or body
- Pass org B's request ID to the request-detail endpoint
- Pass org B's team ID to every mutation — patch, delete, budget reset, key rotate, key revoke
- Page past the end of your own data and check whether another org's rows appear
- Request org B's audit entries directly by ID

Expected: 404 everywhere. **Not 403** — a 403 confirms the resource exists, which is itself a leak.

### Step 4.3 — The subtler vectors

- **Cache**: org A and org B send identical prompts. Does A ever receive B's cached response? Test both orderings.
- **Aggregates**: do cost totals, quality averages, or dashboard counts include any row outside the caller's org? An aggregate that leaks one number is still a leak.
- **Errors**: does a 500 or validation message ever reveal another org's team name, project name, or ID?
- **Token tampering**: modify the org claim in the JWT and replay. Signature verification must reject it.
- **Expired and revoked**: does a revoked refresh token still mint access tokens?
- **Cross-identity**: team key against `:9090`, session cookie against `:8080` — both must fail. Re-verify here even though Phase 1 tested it.

### Step 4.4 — Write it up

`docs/tenant-isolation.md`: what was tested, what the result was, and any accepted residual risk stated plainly.

Automate the highest-value cases as integration tests so a future change can't silently reopen a hole. Cache isolation and cross-org mutation attempts are the two that most warrant permanent tests.

### ✅ Phase 4 checklist

- [ ] Every endpoint tested against cross-org access; all return 404
- [ ] No mutation succeeds against another org's resource
- [ ] Pagination cannot walk into another org's rows
- [ ] **Cache isolation verified in both directions with identical prompts**
- [ ] No aggregate includes out-of-org data
- [ ] No error message leaks another org's names or IDs
- [ ] A tampered JWT org claim is rejected
- [ ] Revoked refresh token cannot mint an access token
- [ ] Cross-identity rejection re-verified both ways
- [ ] Highest-value cases automated as integration tests
- [ ] `docs/tenant-isolation.md` written; residual risks stated
- [ ] `go test -race ./...` clean

**Tag: `multiuser-complete`**

---

## Exit interview

Out loud, from memory.

1. You have two authentication systems. Why, and what stops one being used for the other?
2. Your JWT is in an httpOnly cookie. What attack does that prevent, and what does it introduce?
3. How do you prevent CSRF, and why isn't `SameSite=Lax` enough on its own?
4. Where does the org scope on a query come from, and why not from the request?
5. Before this work, could one user receive another user's cached response? Why, and what did you change?
6. Why is the cache scoped by org rather than by team?
7. Your cache hit rate dropped after scoping. Why is that the right trade?
8. Cross-org access returns 404, not 403. Why does that distinction matter?
9. Someone signs up with Google, then tries email+password with the same address. What happens?
10. A user's JWT is stolen. How long is it useful, and what can they do with it?
11. `is_admin` used to mean admin of everything. What does it mean now, and how did you avoid missing one check?
12. Which single endpoint would you expect to have leaked, and how did you confirm it doesn't?
