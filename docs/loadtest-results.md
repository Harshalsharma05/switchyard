# Load test results

Part 2 rerun of Part 1's k6 load test, extended with a cache-repeat slice and a
routing slice, plus a separate semantic-cache mini-run. Every number below comes
from `scripts/loadtest.js` / `scripts/loadtest-semantic.js` (their own
`handleSummary()` writes `k6-summary.json` / `k6-semantic-summary.json`) and the
gateway's admin API during the exact run — see [Reproduce](#reproduce). Run
against `scripts/loadtest/`'s two mock providers (never a real, billable
completion API); the quality judge and the semantic embedder are the only real
external calls. Developer Windows machine — see [Known gaps](#known-gaps).

## Two Part 1 reporting issues, now fixed

- **`p(95)<10` read `false` in the exported JSON on a run the live output
  passed.** A `--summary-export` quirk in k6 v2.2.0. The scripts no longer use
  `--summary-export`; `handleSummary()` computes the verdict directly from the
  trend value (`overhead_p95_under_10ms`), so the JSON cannot disagree with the
  run that produced it.
- **`http_req_failed` was inflated by the run's deliberate 429/402/503.** The
  scripts now call `http.setResponseCallback(http.expectedStatuses(200, 402,
  429, 502, 503))`, so the metric counts only genuine gateway failures — it
  reads **0.00%** — and the intentional rejections are tallied in their own
  counters. No caption needed; the number is now correct.

## Main run — Part 1 vs Part 2

| | Part 1 | Part 2 |
|---|---|---|
| Requests through the gateway | 5,400 | 5,403 |
| Duration / arrival rate | ~90s @ 60 req/s | ~90s @ 60 req/s |
| Gateway overhead p50 / p90 / p95 | 1.57 / 2.52 / 2.84 ms | 1.87 / 3.23 / 4.03 ms |
| Gateway overhead p99 / max | not reported / 119.16 ms | 8.98 / 118.30 ms |
| `p(95) < 10ms` | pass | **pass** |
| End-to-end latency p50 / p95 | 25.21 / 37.41 ms | 5.50 / 38.52 ms |
| `http_req_failed` | — (metric was misleading) | **0.00%** |
| Checks passed | 5,400 / 5,400 | 5,401 / 5,401 |

Overhead p95 rose ~1.2ms. That is the price of Part 2's hot-path additions: an
exact-tier cache lookup **and** the routing classifier now run on every request,
including the ones that opt out of caching (`X-Switchyard-Cache-TTL: 0`
suppresses storing and serving, not the lookup). Still less than half the 10ms
budget. p99 is 8.98ms — inside budget but close; the tail is worth watching if
more hot-path work lands.

End-to-end p50 fell because ~15% of the run is cache hits served from Redis in
~2ms.

## Status distribution (Part 2)

| Status | Count | Meaning |
|---|---|---|
| 200 | 3,437 | served — directly, from cache, or via fallback |
| 429 | 1,606 | `loadtest-batch`'s 60 rpm / 5,000 tpm enforced under concurrency (Part 1: 1,594) |
| 402 | 147 | `loadtest-budget-capped`'s `$0.02` cap, from a clean counter (see below) |
| 502 / 503 | 211 | outage-window failures for the budget team, whose allowlist forbids the fallback |
| fallback-served | 781 | of the 200s, served by `mock-fallback` after the primary was knocked down 30–60s |

**Why 402s dropped from Part 1's 530 to 147.** The env now flushes Redis before
every run, so the monthly budget counter starts at zero (Part 1's almost
certainly did not — 530 denials out of ~540 budget-team requests implies a
pre-loaded counter). Reconciliation confirms the clean run: `loadtest-budget-
capped` spent **`$0.0198` of its `$0.0200` cap** — 99% — then denied the rest.

**Why the baseline traffic must opt out of caching.** k6's `__ITER` is per-VU
under `constant-arrival-rate`, so `"load test message N"` is really a pool of
~100 strings. Part 1 had no cache, so that pool still hit providers every time.
With Part 2's exact cache on, an earlier run of this script had the cache absorb
the entire workload — budget never exhausted, the outage produced almost no
fallback, every row cost `$0`. So baseline / batch / budget / routed traffic
sends `X-Switchyard-Cache-TTL: 0` and stays provider-bound; only the dedicated
cache slice is cacheable.

## Failover and the circuit breaker

`knockPrimaryDown` at 30s, `restorePrimary` at 60s. During the window the gateway
log shows the expected chain: `mock-primary` passive error rate crosses the hard
threshold → health `healthy → down`, the breaker for `mock-primary` +
`mock-frontier` opens, and subsequent requests are rejected at the breaker
("circuit breaker is open for this provider and model") *before* a provider call
— then failed over. 781 requests carried `X-Switchyard-Fallback: true`.

The budget-capped team (allowlist: `mock-primary` only) still hard-fails on the
outage — its single permitted candidate is behind the open breaker and the
frontier fallback is forbidden. That is allowlist-beats-availability, unchanged
from Part 1.

## Cache slice

24-prompt pool, sent verbatim, `mock-fast`, realtime team. Deliberately small —
this measures the exact tier's mechanics, not a realistic hit rate.

| | |
|---|---|
| Slice requests | 858 |
| Hits / misses | 834 / 24 |
| Hit rate | 97.2% |
| Convergence | 24 misses total — the pool warms in the first few seconds |
| Hit latency (e2e median) | 2.04 ms |
| Miss latency (e2e median) | 34.80 ms |
| Cost saved by cache | **`$0.02502`** over 834 hits (`/admin/attribution`, from logged token counts) |

A hit is ~17× faster end to end than a miss and costs nothing upstream.
`/admin/attribution`'s all-traffic hit rate is 22% — every non-cacheable request
still does a lookup and misses, which drags the ratio down; the meaningful number
is the slice's 97.2%.

## Routing slice

`model: "auto"`, half prompts written simple (short lookups), half written
complex (reasoning verbs + constraints).

| | |
|---|---|
| Routed requests | 592 |
| Classified simple → `fast` (downgraded) | 277 (46.8%) |
| Classified complex → `frontier` | 315 |
| Cost saved by routing | **`$0.02493`** over 277 downgrades (`/admin/attribution`) |

The classifier split the deliberately-simple and deliberately-complex prompts
cleanly (`routing_reason` on the fast rows reads `lookup score=-0.97`). Cache and
routing savings are computed independently and never double-count: a routed
request served from cache records `routing_savings_micros = NULL`.

## Budget reconciliation

`/admin/reconciliation`, period 2026-09, `$0.01` tolerance:

| Team | Redis | Request log | Δ |
|---|---|---|---|
| loadtest-batch | `$0.00135` | `$0.00135` | 0 |
| loadtest-budget-capped | `$0.01980` | `$0.01980` | 0 |
| loadtest-realtime | `$0.13110` | `$0.13110` | 0 |

Exact agreement between the Redis budget counters and the sum over the request
log, all three teams.

## Quality verification

Judge: real Groq `openai/gpt-oss-20b`, concurrency 2, `routed_rate 0.02`.

| | |
|---|---|
| Samples enqueued | 284 (277 downgraded + 7 routed-rate) |
| Scored | 61 |
| Errored | 223 |
| Dropped (queue full) | 0 |
| Mean score | 1.0 / 5 (every one of the 61) |
| Queue depth after the run | 0 |
| Request-path impact | none — `http_req_failed` 0.00%, overhead p95 4.03ms, 5,401/5,401 checks |

Two findings. First, **the judge scored the mock provider's canned string
(`"load test reply from mock-primary"`) 1/5 every time** — the judge is not
rubber-stamping, and `/admin/quality/feedback` correctly lists all scored
downgrades as candidate classifier mislabels. Second, **the real judge could not
keep up**: at ~3 downgrade samples/second against concurrency 2 and Groq's
per-model rate limit, 223 of 284 calls errored. The worker recorded each error
and moved on — nothing queued unbounded, nothing blocked a request. This is
exactly Phase 9's "worker backlog is invisible to callers" guarantee holding
under adverse load, but the scored yield is low; a high-downgrade workload needs
more judge concurrency or a rate cap on downgrade sampling.

## Semantic-cache mini-run

Separate, low-rate, short — every exact-tier miss here pays one real Gemini
embedding round trip. 5 req/s for 60s; 6 canonical prompts primed in `setup()`,
then 65% paraphrases / 35% unrelated, none of them stored.

| | |
|---|---|
| Requests | 242 (+ 6 setup) |
| Gateway overhead p50 / p95 / p99 | 4.23 / 7.31 / 10.31 ms |
| `p(95) < 10ms` with the semantic tier live | **pass** |
| Embedding call p50 / p95 / max | 166.9 / 463.5 / 903.4 ms |
| Semantic hits / misses | 66 / 170 |
| Semantic hit rate (paraphrase traffic) | 41% |

Overhead p95 stays under budget because embedding time is reported on its own
`X-Switchyard-Embed-Ms` header and excluded from `X-Switchyard-Overhead-Ms`, by
the same rule that excludes provider time.

The similarity histogram (`switchyard_cache_similarity`, 213 scored lookups)
separates cleanly: unrelated prompts cluster at or below 0.75, paraphrases at or
above 0.94, and **not one lookup landed in the 0.90–0.93 near-miss band**
(`switchyard_cache_near_miss_similarity_count` is 0). The 0.93 threshold is well
clear of this traffic.

Two caveats on the 41% hit rate. It is sensitive to how tight the paraphrases
are — loosely-worded ones score 0.85–0.92 and miss. And of the 134 lookups that
scored **above** threshold, only 66 served as hits; the rest found a match but
did not return it, most likely the 50ms Redis `read_timeout` timing out the
entry fetch under concurrency. The gateway log's `cache degraded ... reason
redis_read` count would confirm — worth checking before trusting the semantic
hit rate under load.

## Known gaps

- **Not measured from Linux.** Deferred. The Windows monotonic clock has ~529µs
  granularity (Phase 1 `DECISIONS.md`); the p50 1.87ms overhead is well above
  that, but the sub-millisecond tail is not fully trustworthy from this run.
- **Fallback cost delta is `$0` by construction.** `mock-fast-b` and
  `mock-frontier-b` are priced identically to the primaries they back, so
  `/admin/attribution` reports `net_usd 0` for fallback. With real providers
  priced differently the delta would be the price gap × fallback volume.
- **Quality scored yield is 61 / 284** — the real judge is rate-limited under the
  sample burst. Graceful (no request impact) but low.
- **Goroutine / memory return-to-baseline not captured this run** (26 goroutines
  at shutdown; Part 1 verified 19 → 28 → 19 with no leak).

## Reproduce

PowerShell, from the repo root, with Docker Desktop running. `.env` must hold
`GROQ_API_KEY`, `GEMINI_API_KEY`, `POSTGRES_PASSWORD`.

```powershell
# Main run
.\scripts\start-loadtest-env.ps1        # flushes Redis + truncates the request log
k6 run scripts\loadtest.js              # writes k6-summary.json

$H = @{ Authorization = "Bearer sk-loadtest-realtime-9f2b1c" }
irm "http://localhost:9090/admin/attribution?range=24h"   -Headers $H
irm "http://localhost:9090/admin/reconciliation"          -Headers $H
irm "http://localhost:9090/admin/quality/feedback"        -Headers $H
.\scripts\stop-loadtest-env.ps1

# Semantic mini-run
.\scripts\start-loadtest-env.ps1 -Semantic
k6 run scripts\loadtest-semantic.js     # writes k6-semantic-summary.json
.\scripts\stop-loadtest-env.ps1
```

Load-test numbers vary run to run. `k6-summary.json` / `k6-semantic-summary.json`
are regenerated each run and gitignored — this file is the reviewed record.
