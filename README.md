<h1 align="center">SwitchYard</h1>

<p align="center">
  <strong>An LLM API gateway that keeps serving when your provider doesn't.</strong>
</p>

<p align="center">
  Per-team auth, rate limiting, budget enforcement, health-aware failover, circuit breaking,<br/>
  a semantic cache, cost-aware routing, async quality checks, and full observability —<br/>
  behind one OpenAI-compatible endpoint, with a multi-tenant console over all of it.
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white">
  <img alt="Gateway overhead p95" src="https://img.shields.io/badge/gateway%20overhead%20p95-4.03ms-brightgreen">
  <img alt="Genuine failure rate under load" src="https://img.shields.io/badge/http__req__failed-0.00%25-brightgreen">
  <img alt="Race detector" src="https://img.shields.io/badge/go%20test%20--race-clean-brightgreen">
  <!-- <img alt="Dependencies" src="https://img.shields.io/badge/web%20framework-none%20(net%2Fhttp%20%2B%20chi)-blue">
  <img alt="Status" src="https://img.shields.io/badge/status-Part%202%20complete-brightgreen"> -->
  <img alt="License" src="https://img.shields.io/badge/license-MIT-blue">
</p>

![SwitchYard sign-in — Google OAuth on the admin port](web/public/login_page.png)

---

## The problem

The moment more than one team inside a company calls LLM APIs directly, the same four things break at once: nobody knows which team spent the money, one team's runaway retry loop exhausts the shared rate limit for everyone, a provider outage takes down every feature that depends on it with no automatic path to a working alternative, and no one can answer "why was that request slow?" because there is no trace spanning the call. A shared client library fixes none of them — it runs in each service's process and can't enforce a global limit or fail over on policy it doesn't own. SwitchYard sits between your services and OpenAI, Anthropic, and Ollama and solves all four in the request path — while adding **~4 ms** to it. Part 2 layers on a semantic cache, cost-aware routing, async quality verification, and the console.

The multi-user work extends that from "a gateway one team runs" to a system several tenants share. Signing in with Google creates an **organisation**; each organisation owns **projects** (the `teams` table, renamed in the UI), each project owns an API key, limits, and a budget. Every admin query, every cost aggregate, every audit row and every semantic-cache entry is scoped by the organisation on the caller's verified token — never by anything the request supplies. What is *not* yet multi-tenant is the provider credentials: Groq, Gemini, OpenAI, and Anthropic keys are deployment-wide environment variables shared by every organisation, so a tenant's spend is attributed and capped but still billed to the operator's accounts. See [Known gaps](#known-gaps).

A [case study](CASE_STUDY.md) frames the whole project — the problem, the architecture, the numbers, and the one decision worth defending hardest.

## Highlights

| | |
|---|---|
| **Drop-in OpenAI wire format** | Point your existing SDK at SwitchYard by changing one base URL. No client rewrite. |
| **Two identity types, never interchangeable** | Machines authenticate with a project API key on `:8080`. Humans authenticate with a Google session cookie on `:9090`. Neither middleware reads the other's credential, so the rejection is structural rather than a rule that could be relaxed. |
| **Org-scoped everything** | Organisation comes from the signed JWT claim on every admin query — reads, mutations, aggregates, audit, and the cache. A resource in another org answers **404**, never 403. |
| **Per-project keys, limits, and budgets** | Each project gets its own API key, RPM/TPM limits, model allowlist, and monthly USD cap — stored in Postgres and editable live through the admin API. A limit edit or key rotation survives restarts and config reloads. |
| **Token-bucket rate limiting** | Atomic check-and-consume in a single Redis round trip via Lua. Lazy refill, so no background timers and no drift across replicas. Burst-tolerant by design. |
| **Real budget enforcement** | Costs tracked in integer micro-dollars — never floats. Reserve the worst case up front, reconcile against actual usage after. 80% warns, 100% blocks with a `402`. |
| **Health-aware failover** | Active pings plus passive signals from live traffic. Down providers skipped, degraded ones deprioritized, requests fall back down a configured model tier. |
| **Circuit breaker per provider+model** | One bad model doesn't take out a whole provider. Half-open admits exactly **one** probe — held as a distributed lock, so five replicas can't send five "single" probes. |
| **Semantic cache** | Two tiers: an exact Redis-key lookup (sub-millisecond) then a Gemini-embedded nearest-neighbour search. Identical prompts hit instantly; paraphrases hit at a tuned similarity threshold. Fails to a miss, never an error. Keyed per organisation. |
| **Cost-aware routing** | A lexical complexity classifier (microseconds, never an LLM call) routes `model: "auto"` requests to the cheapest capable tier. An explicitly named model is never silently downgraded. |
| **Async quality verification** | An LLM-as-judge scores a sampled slice of routed and cached responses — entirely off the request path, drops the sample rather than blocking. |
| **Streaming stays streaming** | SSE chunks flush as they arrive, never buffered — through the gateway and through nginx. Client disconnect cancels the upstream call. A cached stream is replayed as chunks. |
| **Compliance over availability** | A project is *never* routed to a provider its allowlist forbids — even when that provider is the only healthy one left. It gets an error instead. |
| **Survives its own bugs** | Panic recovery wraps the request path outermost (auth included) and every long-lived background goroutine, which restarts under capped exponential backoff. A panic costs one request, not the process. |
| **Observability + a console** | OpenTelemetry traces (retries and fallbacks as distinguishable sibling spans), Prometheus on a separate admin port, provisioned Grafana, a durable request log in Postgres, and a React console over the lot. |

Two rules are load-bearing throughout: **the gateway must never be the reason a request fails** — every dependency has an explicit fail-open or fail-closed behavior, with budget enforcement the one deliberate fail-*closed* exception — and **telemetry never blocks a request**. Authentication is the third: it fails *closed* in every condition, because an auth bypass is not a latency problem.

## The numbers

From [`docs/loadtest-results.md`](docs/loadtest-results.md), generated by committed scripts anyone can rerun. Nothing is hand-typed.

| Metric | Part 1 | Part 2 |
|---|---|---|
| **Gateway overhead p95** | 2.84 ms | **4.03 ms** (p50 1.87 · p99 8.98) — target < 10 ms, now with a cache lookup and the classifier on every request |
| **Genuine failure rate under load** | metric was misleading | **0.00%** — the deliberate 429/402/503 are now scoped out with `expectedStatuses` |
| **Requests / rate** | 5,400 | 5,403 over ~90 s at a sustained 60 req/s |
| **Rate-limit rejections** | 1,594 × 429 | 1,606 × 429 — unchanged behaviour |
| **Failover under load** | 1,118 | 781 requests served by the fallback provider during a 30 s induced primary outage |
| **Cache slice hit rate** | — | 97.2% — a hit is ~2 ms end to end vs ~35 ms for a miss |
| **Cost saved by cache / by routing** | — | $0.0250 / $0.0249, measured separately from logged token counts |
| **Redis spend vs request-log sum** | — | exact agreement, all three teams |
| **Semantic embedding call** | — | p50 167 ms / p95 464 ms (real Gemini) — excluded from the overhead number, reported on its own header |

Overhead is measured *inside* the gateway and excludes provider and embedding time — reported on every response as `X-Switchyard-Overhead-Ms`, not just under test.

**Tier 1 and the multi-user work were required not to move that number, and didn't.** Team lookup moved to Postgres but never reached the request path — auth reads an in-memory snapshot behind an `atomic.Pointer`, rebuilt by a 30 s background ticker, so a completion does zero database I/O for authentication. Org scoping is entirely control-plane. Re-measured after org scoping landed with [`scripts/overheadbench`](scripts/overheadbench): **p95 4.59 ms** against the 10 ms budget (2.02 ms p50 on a cache hit, 3.17 ms on a miss).

**On the cache hit rate.** The cache was already keyed per *team* before this work, so scoping it to the organisation **widened** sharing rather than narrowing it — one person's several projects now stop paying twice for the same answer, and the measured hit rate should rise, not fall. There was never a cross-org hit to eliminate; the isolation the plan called a security fix was already in place, and [`docs/tenant-isolation.md`](docs/tenant-isolation.md) proves it empirically in both orderings. The 97.2% slice figure above is a pre-scoping, team-scoped measurement and stands as such — **the load test has not been rerun since, so there is no post-scoping figure to quote**, and a number nobody measured is not going in this file. A per-project `cache_isolated` flag can narrow a project back out of its org's pool; it can never widen sharing.

Honest caveats, stated not buried: the load test still runs on **Windows** (Linux deferred; the ~529 µs monotonic-clock granularity makes the sub-millisecond tail less trustworthy); **60 req/s is a sustained rate, not a measured throughput ceiling**; the **fallback cost delta is $0** because the mock fallback models are priced identically to their primaries; and the **quality judge scored 61 of 284 samples** under the real provider's rate limit — graceful (zero request impact), but low yield.

## Architecture

### System

```mermaid
flowchart LR
    SDK["Machines<br/>any OpenAI SDK"] -->|"project API key"| MW
    UI["Humans<br/>React console"] -->|"session cookie + CSRF"| ID

    subgraph GW["SwitchYard"]
        direction TB
        subgraph DP["data plane — :8080 · project keys only"]
            MW["Middleware chain<br/>auth · limits · budget"]
            CA["Semantic cache<br/>keyed by organisation"]
            RT["Routing + resilience<br/>classify · health · retry · fallback · breaker"]
            MW --> CA --> RT
        end
        subgraph CP["control plane — :9090 · sessions only"]
            ID["Google OAuth → JWT<br/>user · org · superadmin"]
            AD["Admin API<br/>every query filtered by the org claim"]
            ID --> AD
        end
    end

    RT --> P1["OpenAI · Groq"]
    RT --> P2["Anthropic · Gemini"]
    RT --> P3["Ollama<br/>local · free"]

    RT <-->|"limits · budgets · breaker · cache"| RD[("Redis")]
    RT -->|"request log (post-response)"| PG[("Postgres")]
    AD <-->|"orgs · users · sessions<br/>projects · audit · request log"| PG
    RT -.->|"sample"| QW["Quality worker<br/>async LLM-as-judge"]
    QW -->|"score"| PG
    MW -.->|"metrics"| PM["Prometheus → Grafana"]
    MW -.->|"traces"| JG["Jaeger"]
```

The two ports are the tenancy boundary. `:8080` reads `Authorization` and never looks at a cookie; `:9090` reads the session cookie and never looks at `Authorization`. The console is the only client that talks to both — its dashboard screens use the session, and its Playground and load simulator post completions to `:8080` with a project key pasted in separately, because signing in does not authorise a completion.

Dotted lines are telemetry: they can fail, hang, or disappear entirely without affecting a request. The quality worker runs off the request path — its input is a post-response sample, its failure is invisible to callers.

### Request path

Defined in exactly one place, [`internal/proxy/router.go`](internal/proxy/router.go). Middleware first (needs only the token), then the handler (needs the decoded body). The hot path never learned about users: it resolves a project key to a project, and the organisation only ever enters as the cache scope.

```mermaid
flowchart TB
    R(["Request"]) --> A["Recoverer · RequestID · Timing · Tracing · Logger · Metrics"]
    A --> G["Auth — bearer token → project<br/>(in-memory snapshot, no DB round trip)"]
    G --> H["RateLimit · RPM"]
    H --> RT["Route — classify a model: auto request to a tier<br/>(an explicitly named model passes straight through)"]
    RT --> AZ["Authorize model against the project's allowlist"]
    AZ --> CH["Cache lookup — org-scoped, exact then embedded NN<br/>hit ⇒ serve, no reservation, no provider call"]
    CH --> TPM["TPM reservation"] --> BUD["Budget reservation"] --> RES(["Resolve model → fallback chain ↓"])
```

Routing runs *before* authorization because it decides which model is being authorized. The cache is consulted *after* authorization and *before* any reservation — a hit spends no tokens and no money, so it must not draw down a bucket. `Recoverer` is outermost deliberately: a panic inside auth itself would escape anything sitting behind it.

### Resilience path

```mermaid
flowchart TB
    CH["Fallback chain<br/>allowlist-filtered, health-ordered"] --> S
    S{"Candidate<br/>left?"} -->|"no"| E503["503 with a per-candidate<br/>breakdown of what failed"]
    S -->|"yes"| BR{"Breaker<br/>open?"}
    BR -->|"open"| SKIP["Skip instantly<br/>no call, no timeout wait"] --> S
    BR -->|"closed"| CALL["Call provider"]
    CALL --> OUT{"Outcome"}
    OUT -->|"success"| WIN["Return · record cost, usage,<br/>routing savings, quality sample"]
    OUT -->|"retryable"| RT["Backoff, full jitter<br/>honors provider Retry-After"] --> CALL
    OUT -->|"caller's fault"| FAIL["Return the error<br/>no fallback: it fails everywhere"]
    OUT -->|"provider's fault"| S
```

Retries happen against the *same* provider first, then the chain moves on — and never returns to a provider it has given up on. Total attempts are capped across the entire chain, so a 3-retry policy against a 5-entry tier can't become 15 calls against an already-struggling fleet.

## Quickstart

> **Google OAuth is a prerequisite, not a footnote.** Since the admin port accepts only a session, a clone without a working Google OAuth client cannot sign in to the console at all — and the gateway now **refuses to start** without `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and `JWT_SECRET`, because a process that boots healthy while every `/admin` route 401s cost an afternoon once already. Do step 1 before anything else.

**Requires:** Docker, a Google account, [Ollama](https://ollama.com) running locally (optional — see below), and a Groq **and** Gemini API key (both free tiers — Gemini also powers the cache's embeddings). Go 1.26+ only to run the gateway outside Docker.

### 1. Create a Google OAuth client

In [Google Cloud Console](https://console.cloud.google.com/):

1. Create a project, then configure the **OAuth consent screen** — User type *External*, publishing status *Testing*. Add the Google account you intend to sign in with as a **test user**; in Testing mode nobody else can sign in, capped at 100 addresses.
2. **Credentials → Create credentials → OAuth client ID**, application type *Web application*.
3. Register the **authorised redirect URI**:

   | Running via | Redirect URI to register |
   |---|---|
   | `docker compose` (console on :3001) | `http://localhost:3001/auth/google/callback` |
   | Vite dev server (console on :5173) | `http://localhost:5173/auth/google/callback` |

   **It must be the console's origin, not the gateway's admin port.** The gateway's own built-in default (`http://localhost:9090/auth/google/callback`) does not work for a browser sign-in: the callback finishes with a *relative* redirect to `/signing-in`, which only the console serves — land the callback on `:9090` and the browser ends on a 404 holding a valid session it can't use. Both the Vite dev proxy and the production nginx config forward `/auth/*` to the admin port for exactly this reason, so sign-in is first-party to the console origin.
4. Copy the client ID and secret. They go in `.env`, never in YAML, never committed.

### 2. Fill in `.env`

```powershell
Copy-Item .env.example .env
```

| Variable | Why it's needed |
|---|---|
| `GROQ_API_KEY`, `GEMINI_API_KEY` | The two enabled providers. Gemini also embeds for the semantic cache. |
| `POSTGRES_PASSWORD` | Projects, users, organisations, sessions, audit, and the request log all live in Postgres. The gateway refuses to start without it. |
| `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` | From step 1. Fatal at startup if unset. |
| `JWT_SECRET` | Signs the session cookie. `openssl rand -base64 48`. Fatal if unset; rotating it invalidates every session. |
| `SWITCHYARD_OAUTH_REDIRECT_URL` | Must match what you registered in step 1 **exactly**, scheme and port included. The default points at `:9090` and will not work — set it. |
| `SWITCHYARD_SUPERADMIN_EMAIL` | The Google address that gets superadmin and adopts the seeded default organisation. Any other address gets an ordinary user in a brand-new organisation of its own, and the superadmin-only screens (System, chaos, reload, breaker reset, cache tuning) stay closed. Set it to the address you will actually sign in with. |

### 3. Bring the stack up

```powershell
docker compose -f deploy/docker-compose.yml up -d --build
```

Brings up the gateway, Redis, Postgres (schema migrated and `configs/teams.yaml` seeded by the one-shot `migrate` service), Prometheus, Jaeger, Grafana, and the console. Ollama runs natively on the host; the gateway reaches it at `host.docker.internal:11434`.

| | |
|---|---|
| Console | [localhost:3001](http://localhost:3001) — **Continue with Google** |
| Gateway (public) | `localhost:8080` |
| Grafana | [localhost:3000](http://localhost:3000) |
| Jaeger | [localhost:16686](http://localhost:16686) |

### 4. Send a request

The gateway is authenticated separately from the console, with a project key — the seeded dev key works out of the box:

```powershell
curl.exe http://localhost:8080/v1/chat/completions `
  -H "Authorization: Bearer sk-switchyard-dev-acme-9f2b1c" `
  -H "Content-Type: application/json" `
  -d '{\"model\":\"openai/gpt-oss-20b\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'
```

The response carries `X-Switchyard-Overhead-Ms`, `-Provider`, `-Served-Model`, `-Cache`, and — when routing ran — `-Route-Tier` / `-Route-Reason`.

#### One-time: pull a model for the local fallback

The `fast` tier's last hop is Ollama — local, free, no credential — but pulling a model is a real multi-gigabyte download nothing can bake in:

```powershell
ollama pull llama3.2:3b
```

Skip it and everything else works: Groq and Gemini serve normally, and the health checker marks `ollama` unavailable rather than blocking — the only gap is the final fallback hop having nothing to serve if every paid provider is also down.

## Security model

Short by design. The reasoning behind each choice — and the alternative it beat — is in [DECISIONS.md](DECISIONS.md).

**Two identity types, structurally separate.** A project API key authenticates a machine on `:8080`. A session cookie authenticates a human on `:9090`. They are not kept apart by a rule that rejects the wrong credential — `internal/proxy` contains no reference to cookies and the session middleware never reads `Authorization`, so there is nothing to reject. This matters more than it sounds: cookies ignore port, so a browser signed in to the console genuinely does attach `sy_session` to a `localhost:8080` request in development, and the gateway must actively not care. Both directions are integration-tested against the real compiled binary on both real ports.

**Scope comes from the token, never the request.** Every org-scoped query derives its organisation from the verified JWT claim in the request context. A handler that read an org or project ID from a query parameter, path segment, or body and trusted it would be a tenant-isolation bug regardless of what the UI sends — so `?team=` narrows *within* the caller's own scope and is ignored when it names someone else's. Cross-org resources answer **404, not 403**, because a 403 confirms the ID is real.

**What the JWT holds.** User ID, organisation ID, superadmin flag, session ID, issued-at and expiry — and nothing else. A JWT is signed, not encrypted; anyone holding it can read the payload, so no email, name, or key material goes in. It is HS256, hand-rolled, and the verifier never parses the token header at all: a verifier with no branch on an attacker-supplied `alg` cannot be steered into `alg:none` or an RS256-verified-as-HS256 confusion.

**Cookies and CSRF.** The access token rides in an httpOnly, `SameSite=Lax` cookie (`Secure` when the redirect URL is HTTPS), so XSS cannot read it — which is also why session state comes from `GET /auth/me` rather than client-side decoding. The httpOnly cookie introduces CSRF, answered with **signed double-submit**: a JS-readable `sy_csrf` cookie echoed back in an `X-CSRF-Token` header, HMAC'd so someone who can set a cookie cannot forge a matching pair. Not an origin check — the console sits behind two different reverse proxies (Vite with `changeOrigin`, then nginx), and an origin check that silently stops matching is a CSRF defence that fails open with no signal. The long-lived refresh cookie is scoped `Path=/auth`, so the most valuable credential never travels on a dashboard poll.

**Admin roles.** *Org admin* is every signed-in user, over their own organisation's projects, keys, budgets, reconciliation, quality feedback, and audit. *Superadmin* is cross-org: config reload, chaos, breaker resets, the system panel, and the cache sweep, which aggregates across every tenant's buckets. The old team-level `is_admin` flag grants nothing now — it survives only because the console's create form still sends it.

## Multi-tenancy

**Organisation → projects → keys.** Signing in with Google creates a user and an organisation in one transaction; a user without an organisation is a broken state nothing downstream has to handle. The organisation owns projects — the same `teams` table, relabelled in the UI, because "project" is the honest word for a solo user with three side projects. Each project carries its own API key, allowlists, RPM/TPM limits, and monthly cap, and Usage & Cost rolls every project in the organisation into one view with a top-bar project selector that scopes Overview, Request Logs, and Usage & Cost alike (selection lives in the URL, so a filtered view is shareable).

The semantic cache is scoped by **organisation**, not project: one person's projects sharing answers is useful and safe, two tenants sharing them is a leak. A project can opt out with `cache_isolated`, which can only narrow sharing, never widen it.

The audit log is scoped to the organisation whose resources changed — including when a platform operator is the one who changed them. A tenant sees that its key was rotated, with the operator anonymised to `platform-operator` and no actor address; entries belonging to no tenant (config reloads, the bootstrap grants) are visible to the superadmin only.

**[docs/tenant-isolation.md](docs/tenant-isolation.md)** is the evidence. Two real organisations, real sessions, real Postgres and Redis, attacking org A's session with org B's IDs over real HTTP against the compiled binary: every endpoint by path ID, every mutation, pagination past its own end, identical prompts through the cache in both orderings, aggregate totals against known seeds, error bodies checked for the other org's name, a tampered `org` claim, and both cross-identity directions. Every attack is now a permanent integration test, so a future change that reopens a hole fails the suite rather than a human's memory. Residual risks — the semantic tier, cache purge by project, refresh-token revocation at the integration level, timing side channels — are stated there rather than papered over.

## Screens

The console reads only from gateway endpoints — never Prometheus, Postgres, or a provider directly.

![SwitchYard Overview — KPI row, request volume, provider health, circuit breakers](web/public/dashboard_overview.png)

| Screen | What it shows |
|---|---|
| **Overview** | KPI row, traffic and overhead charts, provider-health strip, breaker states, a live request feed — scoped by the project selector |
| **Playground** | A streaming request with the full metadata panel; `429` / `402` / `503` rendered as first-class output. Takes a project API key, separately from the session — it posts to `:8080` |
| **Live Ops** | Provider panel with transition history, the breaker state machine, the chaos harness (including forced panics), a browser load simulator |
| **Request Logs** | The Postgres-backed log, cursor-paginated, server-side filtered, with a Jaeger deep link per row |
| **Usage & Cost** | The org-wide roll-up across every project, then per-project spend vs budget, cost trends, cache and routing savings, the Redis-vs-log reconciliation strip |
| **Settings** | Projects in your organisation: create (key shown exactly once), edit limits, rotate, revoke, delete; plus the System panel |

## Configuration

Provider and routing config is YAML, hot-reloadable via `POST /admin/reload` with no restart and no dropped in-flight requests. A config that fails validation is rejected outright; the running gateway keeps serving on the last good one.

**Projects are not YAML and are not part of a reload.** They live in Postgres. `configs/teams.yaml` seeds an empty database on first boot and is never read again once any project exists — editing it on a running system does nothing at all. This is the fix for the reload bug: before Tier 1, a reload rebuilt the registry from YAML and silently reverted every key rotation and limit edit made through the admin API. Now a rotation or a limit change survives both a restart and a reload, pinned by `TestReloadDoesNotRevertTeamChanges`.

| File | Holds |
|---|---|
| [`configs/providers.yaml`](configs/providers.yaml) | Provider instances (`name` ≠ adapter `type`, so a free stand-in swaps for the real vendor by editing this file alone) and the fallback tiers that chain them. A model belongs to at most one tier. Deployment-wide, not per-organisation. |
| [`configs/teams.yaml`](configs/teams.yaml) | **First-boot seed only.** Imported into Postgres by `cmd/migrate` when the `teams` table is empty, never read again after. Per-project key hash (SHA-256; plaintext never stored), allowlists, RPM/TPM, monthly USD cap, priority, `cache_isolated`, and an `is_admin` that no longer grants anything. |
| [`configs/cache.yaml`](configs/cache.yaml) | Cache on/off, the two-tier split, similarity threshold, per-content TTL rules, embedding source, per-scope entry cap. |
| [`configs/router.yaml`](configs/router.yaml) | Complexity level → tier, and the classifier's weights, saturation scales, and lexicons. |
| [`configs/quality.yaml`](configs/quality.yaml) | Sampling policy, the judge model, worker concurrency and queue size. |

<details>
<summary><strong>Key environment variables</strong> — defaults shown; the six marked <strong>required</strong> are fatal at startup</summary>

| Variable | Default | Purpose |
|---|---|---|
| `SWITCHYARD_ADDR` / `SWITCHYARD_ADMIN_ADDR` | `:8080` / `:9090` | Public and admin listeners — never expose the admin port publicly |
| `SWITCHYARD_REDIS_ADDR` | `localhost:6379` | Limits, budgets, health, breaker state, cache |
| `POSTGRES_PASSWORD` | — | **Required.** Projects, users, orgs, sessions, audit, request log |
| `SWITCHYARD_POSTGRES_HOST` | `localhost:5432` | Database host |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | — | **Required.** Dashboard sign-in; without them the admin port has no way to authenticate a human |
| `JWT_SECRET` | — | **Required.** Signs the session JWT and the CSRF and OAuth-state cookies |
| `SWITCHYARD_OAUTH_REDIRECT_URL` | `http://localhost:9090/auth/google/callback` | Must match Google Cloud Console exactly — **and must be the console's origin, not `:9090`** |
| `SWITCHYARD_SUPERADMIN_EMAIL` | — | Address granted superadmin at sign-in. Unset means no superadmin ever exists |
| `SWITCHYARD_ACCESS_TOKEN_TTL` / `SWITCHYARD_REFRESH_TOKEN_TTL` | `15m` / `720h` | Access-token lifetime is also the post-revocation window; refresh rotates on every use |
| `SWITCHYARD_TEAM_REFRESH_INTERVAL` | `30s` | Project snapshot refresh — also the key-revocation window on any replica that did not make the change |
| `GEMINI_API_KEY` | — | The semantic cache's embedding source (fatal at startup if the cache is on and this is unset) |
| `SWITCHYARD_{CACHE,ROUTER,QUALITY}_CONFIG` | `configs/*.yaml` | Override each config path |
| `SWITCHYARD_ENV` / `SWITCHYARD_CHAOS_ENABLED` | `production` / `false` | `dev` + the flag together unlock the chaos harness |
| `SWITCHYARD_PROMETHEUS_URL` | `http://localhost:9091` | Queried server-side for `/admin/summary` |
| `SWITCHYARD_DRAIN_TIMEOUT` | `25s` | Graceful shutdown window on SIGTERM |

Retry, breaker, and health thresholds are all env vars too — see [`.env.example`](.env.example).

</details>

## API

### Public — port 8080 (project API key; no cookie is ever read here)

| Endpoint | Notes |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible. Streaming and non-streaming share one middleware chain. `model: "auto"` opts into routing. `X-Switchyard-Cache-TTL: 0` opts a request out of caching. |
| `GET /healthz` · `GET /readyz` | Liveness (checks nothing) · readiness (503 only if config failed to load or **every** provider is down). |

**Response headers:** `X-Switchyard-Request-Id` · `-Provider` · `-Overhead-Ms` · `-Requested-Model` · `-Served-Model` · `-Fallback` · `-Cache` · `-Embed-Ms` · `-Route-Tier` · `-Route-Reason` · `-Budget-Warning` · `X-RateLimit-*` · `Retry-After`

### Sign-in — port 9090

| Endpoint | Purpose |
|---|---|
| `GET /auth/google` · `GET /auth/google/callback` | Authorization-code flow with PKCE; `state` and verifier ride in a short-lived signed cookie. A missing or mismatched `state` is a flat 400 — it's a forged callback, not a typo |
| `GET /auth/me` | The signed-in user, their organisation, superadmin flag, and `session_expires_at` (which is what lets the console schedule a refresh it can't read from an httpOnly cookie) |
| `POST /auth/refresh` · `POST /auth/logout` | Rotate the refresh token (reuse revokes every session for that user); log out, clearing cookies and revoking server-side |

### Admin — port 9090 (session cookie + CSRF; org-scoped from the claim)

| Endpoint | Purpose |
|---|---|
| `GET /admin/summary` | Overview's data — Prometheus queried server-side, `1h`/`24h`/`7d`/`30d` |
| `GET /admin/requests` · `/admin/requests/{id}` | The request log — cursor-paginated, filterable, scoped to the caller's organisation |
| `GET /admin/costs` · `/admin/attribution` | Cost trends; cache and routing savings, and the cost shifted by fallback |
| `GET /admin/reconciliation` | Redis budget counters cross-checked against the request-log sum |
| `GET /admin/quality/feedback` | The cache-threshold and classifier feedback loops |
| `GET /admin/providers` · `/admin/providers/health` | Configured providers; per-provider status and transition history |
| `GET`·`POST /admin/teams` · `GET`·`PATCH`·`DELETE /admin/teams/{id}` | Projects in your organisation. Create returns the plaintext key exactly once; another org's ID is a 404 |
| `POST /admin/teams/{id}/reset-budget` · `POST .../key/rotate` · `DELETE .../key` | Budget reset, key rotation, key revocation — all persisted, all audited before the mutation |
| `GET /admin/audit` | Who changed what in your organisation, operator actions included, operator identity anonymised |
| `DELETE /admin/cache` | Purge one project *(org admin)*; purge by prefix or purge everything *(superadmin)* |
| `POST /admin/providers/{name}/breaker/reset` · `POST /admin/reload` | Manual breaker intervention; config reload *(superadmin)* |
| `GET /admin/system` · `GET /admin/cache/tune` | Deployment facts and dependency reachability; threshold sweep over historical requests *(superadmin)* |
| `GET`·`POST`·`DELETE /admin/chaos` | Fault injection, including forced panics — refuses unless `SWITCHYARD_ENV=dev` **and** the flag is set *(superadmin)* |
| `GET /metrics` | Prometheus exposition (unauthenticated, admin port only) |

A valid project API key reaches **none** of the `/admin` surface — verified live, and re-verified in the isolation audit. Committed scripts that drive the admin port (`scripts/demo.sh`, the integration harness) mint an ordinary session with [`cmd/admintoken`](cmd/admintoken), which needs `JWT_SECRET`; it is not a second authentication path, since anyone holding that secret could forge a session by hand anyway.

## Use cases

**Internal LLM platform for multiple teams.** Per-project key, allowlist, and monthly cap. Finance gets per-project attribution for free; no project can exhaust another's quota.

**Several tenants on one gateway.** Each organisation signs in with Google, sees only its own projects, logs, costs, and audit trail, and cannot reach another's by any request it can construct — proven, not asserted.

**Surviving provider outages.** Health checks and the breaker route around a degrading provider automatically, down to a local Ollama model that costs nothing.

**Cutting spend without cutting features.** Repeated and paraphrased prompts are served from the semantic cache; `model: "auto"` requests route to the cheapest tier that can handle them; the async judge keeps both trades honest by scoring a sample of what they produced.

**Escaping vendor lock-in.** The public API mirrors OpenAI's wire format and every provider sits behind one interface — switching or adding a provider is a YAML edit. Clients never change.

**Debugging "why was that request slow?"** One trace per request, retries and fallbacks as distinguishable sibling spans, the trace ID in every log line and deep-linked from every Request Logs row. Prompt and response content is *never* attached to spans or the log.

## Testing

```powershell
go test -race ./...                       # unit + package tests; the race detector is non-negotiable
go test -race -tags=integration ./test/   # black-box: needs Redis and Postgres (POSTGRES_PASSWORD set)
```

The integration suite compiles `cmd/gateway` and `cmd/migrate`, seeds each test's projects into a throwaway Postgres database, spawns the gateway against it, real Redis, and mock upstreams, and drives it purely over HTTP — never importing internal packages. It proves the shipped artifact behaves: exact rate limiting under concurrency, budget cut-off, retry classification, fallback with allowlist enforcement, the full breaker cycle, progressive streaming, client-cancel propagation, hot reload without dropping an in-flight request, and the whole tenant-isolation attack set in [docs/tenant-isolation.md](docs/tenant-isolation.md).

Unit tests concentrate on the state machines and failure paths — the token bucket, the breaker, the health hysteresis, the classifier, the sampler, the middleware chain, the JWT verifier, the session lifecycle. Packages that are mostly HTTP/Redis/Postgres glue (`internal/cache`, `internal/logstore`) are covered by the integration and load tests rather than by unit tests against a mock. To rerun the load test, see [`docs/loadtest-results.md`](docs/loadtest-results.md).

## Demo

[`scripts/demo.sh`](scripts/demo.sh) narrates the whole system in ten scenes against the running compose stack, pausing before each and printing the parallel "do this in the console" steps: live Overview, a streaming request, a cache hit, cost-aware routing, the failure chain reaction (health degrades → breaker opens → fallback engages), recovery, a rate-limit wall, a budget wall, Request Logs filtered to the failures with a Jaeger deep link, and Usage & Cost showing real savings. It does every API action itself, so each scene is deterministic and doubles as a smoke test; the preflight fails fast if a piece of the stack is missing, and it mints its own admin session with `cmd/admintoken`.

```powershell
docker compose -f deploy/docker-compose.yml up -d --build
bash scripts/demo.sh
```

## Project layout

```
cmd/gateway/           entrypoint, wiring, graceful shutdown, hot reload
cmd/migrate/           one-shot schema migration + first-boot team seed
cmd/admintoken/        mints a session so committed scripts can drive :9090
internal/config/       YAML loading and validation
internal/provider/     Provider interface + OpenAI/Anthropic/Gemini/Ollama adapters
internal/proxy/        middleware chain, request lifecycle, streaming, cache/routing wiring
internal/auth/         project key validation and context (pure in-memory, no I/O)
internal/teamstore/    Postgres-backed project store + the snapshot that keeps auth off the hot path
internal/identity/     users, orgs, sessions, JWT, CSRF, session middleware
internal/oauth/        Google authorization-code flow
internal/ratelimit/    token bucket — Redis Lua, hand-written
internal/budget/       cost accounting and spend caps
internal/health/       active pings + passive signals + status hysteresis
internal/resilience/   retry, backoff, fallback chains, circuit breaker
internal/cache/        two-tier semantic cache
internal/router/       complexity classifier + cost-aware routing policy
internal/quality/      async LLM-as-judge worker and sampler
internal/logstore/     Postgres request-log writer, query layer, retention, audit
internal/summary/      Prometheus query layer behind GET /admin/summary
internal/telemetry/    OTel setup, span helpers, Prometheus metrics, goroutine supervision
internal/admin/        admin API handlers, org scoping, superadmin gate
migrations/            versioned SQL — request log, audit, teams, identity, audit scope
web/                   React console (Vite) + its production nginx config
deploy/                Compose stack, Prometheus config, provisioned Grafana
scripts/               load tests, mock providers, demo, environment setup
test/                  black-box integration suite, including the isolation attacks
```

`internal/provider/` knows nothing about projects, limits, or budgets. `internal/proxy/` is the only package that knows the full request lifecycle, and nothing imports it except `cmd/`. `internal/admin/` and `internal/identity/` do not import each other — `cmd/` supplies the adapter between them, so the import graph enforces the identity boundary rather than a convention doing it. Interfaces are defined by the consumer.

## Design decisions

Every non-obvious choice — and the alternative it was chosen over — is in **[DECISIONS.md](DECISIONS.md)**, one section per phase across Part 1, Part 2, Tier 1, and the multi-user work: why token bucket over sliding window, why rate limiting fails open while budgets and auth fail closed, why one probe in half-open, why the cache is two tiers with the exact one first, why the classifier never calls a model, why a project's allowlist beats provider availability, why the team snapshot is refreshed wholesale rather than cached lazily, why the JWT verifier reads no header, why CSRF is double-submit rather than an origin check, and why an operator's action on your project is visible to you but their identity is not. The [case study](CASE_STUDY.md) picks the one worth defending hardest.

## Failure modes

Every fail-open/fail-closed claim above is asserted with intent, not just tested in passing. **[docs/failure-modes.md](docs/failure-modes.md)** is the evidence: every dependency (Redis, Postgres, Jaeger, Prometheus, the embedding source, every provider, the quality worker) stopped one at a time and in combination against the live stack, with what a user sees, what an operator sees, and what recovers automatically versus what needs intervention. It also states the one honest exception plainly: a sustained Redis outage takes chat completions to 0% success, because budget's fail-closed check runs before every piece of resilience machinery — the rate limiter's fail-open design and the breaker's Redis-free local state are real, but currently unreachable when Redis is fully down. Postgres down is far gentler: projects already in the snapshot keep authenticating, unknown keys still get a clean 401, and the System panel reports `team_store: degraded` separately from `postgres: down` so it's visible that requests never stopped. One bug the audit found (`/admin/summary` hanging 27 s when Prometheus is down) is fixed there too.

## Known gaps

Named rather than omitted. Each is a real limitation of what is built today.

- **It runs locally only.** `docker compose` on one machine. There is no deployment, no TLS termination, no hosted anything, and the numbers above come from a developer Windows box.
- **No per-organisation provider keys.** Provider credentials are deployment-wide environment variables. Every tenant's traffic goes through the operator's Groq/Gemini/OpenAI accounts — spend is attributed and capped per project, but billed to one set of keys. A genuine multi-tenant product needs bring-your-own-key, and this does not have it.
- **Google is the only way in, and there is no password reset.** Email+password sign-in was cut deliberately: two credential types on one address is an account-linking attack surface that cannot be closed safely without verified email delivery. That also means dashboard sign-in depends on a third party being reachable, and the consent screen is in Testing mode — 100 hand-registered test users, maximum. `:8080` is untouched by any of this.
- **One user per organisation.** No invites, no members, no roles beyond org-admin and superadmin. The schema supports more; nothing else does.
- **Revocation is not instant, in either identity system.** A revoked session's **access token stays valid for up to 15 minutes**, because `sid` is carried in the claims but is not checked against the database per request — logout and reuse detection revoke server-side immediately, so no *new* token can be minted, but one already in a browser keeps working until it expires. A revoked or rotated **project API key stays valid for up to 30 seconds** on any replica that did not make the change (zero on the one that did), because the auth snapshot refreshes on a ticker rather than by push. Both numbers are the accepted cost of keeping a database round trip off the request path; neither is "instant."
- **`SWITCHYARD_SUPERADMIN_EMAIL` is a bootstrap mechanism with a continuous blast radius.** It is matched on *every* sign-in, not just the first — which is what makes a typo recoverable, and also means whoever can edit the environment can grant themselves superadmin at their next login, for as long as the gateway runs. Superadmin is never removed automatically, so the flag accumulates: every address that has ever held that value while signing in keeps it until someone deletes it by hand in SQL. Acceptable while the operator and the deployer are the same person. **It needs revisiting before anyone else deploys this** — superadmin should be granted by an authenticated action, not by an env var read at every login.
- **Ollama is not in Compose.** It runs natively on the host and the gateway reaches it at `host.docker.internal:11434`. Without it the `fast` tier simply loses its last, free hop.
- **The load-test numbers predate the multi-user work**, and `docs/loadtest-results.md`'s reproduce block still drives the admin API with a project key on `:9090`, which now returns 401. The k6 runs themselves are unaffected — they only touch `:8080` — but the admin reads in that block need `cmd/admintoken` before they work again.
- **Residual isolation risks** — the semantic tier as opposed to the exact tier, cache purge by project at the integration level, refresh-token revocation end to end, timing side channels, and a forced 500 — are listed in [docs/tenant-isolation.md](docs/tenant-isolation.md). They are boundaries the audit did not reach, not failures inside one it did.

## Out of scope

Kubernetes, Terraform, and cloud deployment — all deliberately.

## License

MIT — see [LICENSE.md](LICENSE.md).
