# Load test results

Every number below is from `k6-summary.json`, produced by the exact command
in [Reproduce](#reproduce) — nothing here is hand-typed. Run against
`scripts/loadtest/providers.yaml`'s two mock providers (never a real,
billable API), on the developer's Windows machine (see
[Known gaps](#known-gaps) for what that does and doesn't prove).

## Summary

| | |
|---|---|
| Total requests through the gateway | 5,401 |
| Duration | ~90s at a fixed 60 req/s arrival rate |
| Gateway overhead, p50 / p90 / p95 | 1.55ms / 2.58ms / 3.11ms |
| Gateway overhead, max | 141.32ms (see [below](#gateway-overhead)) |
| End-to-end latency, p50 / p95 | 25.07ms / 37.95ms |
| Checks passed | 5,401 / 5,401 (100%) — every response was a status the gateway is meant to produce |

## Status code distribution

| Status | Count | Share | Meaning |
|---|---|---|---|
| 200 | 2,486 | 46.0% | Served, directly or via fallback |
| 429 | 1,638 | 30.3% | `loadtest-batch`'s deliberately tight RPM/TPM being enforced under real concurrency |
| 402 | 480 | 8.9% | `loadtest-budget-capped`'s $0.02 cap being enforced |
| 502 / 503 | 797 | 14.8% | `loadtest-budget-capped` requests during the primary-outage window — that team's only allowed model has no fallback tier, so a down primary is a hard failure for it by design (allowlist beats availability, Phase 6) |

Of the 2,486 successes, **754 (30.3%)** carried `X-Switchyard-Fallback: true`,
served by `mock-fallback` — these landed inside the run's 30s–60s window,
during which `scripts/loadtest.js`'s `knockPrimaryDown` scenario held
`mock-primary` down. The other two teams' models both sit in the `fast`
fallback tier, so their traffic kept succeeding through the outage; only
the budget-capped team's single-candidate model was actually exposed to it.
Restoring `mock-primary` at 60s is what let the 200/fallback split settle
back down for the run's last third.

This run does not re-prove rate-limit *exactness* under concurrency — that's
the job of `TestRateLimitIsExactUnderConcurrency` in `test/ratelimit_test.go`,
which asserts an exact admitted count against a fixed RPM. What this run adds
is the same enforcement holding up under real, sustained, mixed-team
concurrency rather than one isolated burst.

## Gateway overhead

`gateway_overhead_ms` is read from `X-Switchyard-Overhead-Ms` on every
response — measured inside the gateway itself, excluding provider time, per
Step 1.6. p50/p90/p95 (1.55 / 2.58 / 3.11ms) are comfortably under the
<10ms target.

The 141.32ms max is not noise: a request that retries against a down
`mock-primary` (3 attempts, full-jitter backoff) before falling over to
`mock-fallback` spends that entire retry-and-backoff window inside the
handler, which is legitimately gateway overhead by the Step 1.6 definition —
it is not provider time, and the fallback candidate's own latency is what
gets subtracted, not the failed attempts against the primary. The outlier is
concentrated in the 30s–60s primary-outage window for exactly this reason.

The exported JSON reports `"p(95)<10": false` under `thresholds` for this
metric, which contradicts the p(95) value of 3.11ms sitting in the same
object. Recorded here rather than silently corrected — this looks like a k6
summary-export reporting quirk rather than a real threshold breach, but it's
flagged, not asserted away.

## Known gaps

- **Not measured from Linux.** Per Phase 1's `DECISIONS.md`, this Windows
  dev machine's monotonic clock has ~529µs granularity, which is fine for
  values in the 1–3ms range reported here but means the true tail behavior
  under sub-millisecond load is not fully trustworthy from this run alone.
  A rerun inside the Linux container this project already builds
  (`cmd/gateway`) would make the number fully defensible.
- **Not a throughput ceiling.** The run held a fixed 60 req/s arrival rate
  (`constant-arrival-rate`); `vus_max` topped out at 52 of a 200 budget,
  meaning the gateway never had to work hard to keep up. This proves the
  gateway sustains 60 req/s cleanly, not what its actual ceiling is. Finding
  that needs a follow-up run that ramps the rate until overhead or error
  rate breaks down.
- **Goroutine and memory counts are missing.** `scripts/start-loadtest-env.ps1`
  and `scripts/stop-loadtest-env.ps1` print `go_goroutines` before and after
  the run specifically to fill in the plan's "no goroutine leaks" checklist
  item, but that console output wasn't captured alongside `k6-summary.json`
  this time. Paste those two "Baseline goroutines" / "Goroutines before
  stopping" lines next run and this section gets filled in for real, rather
  than guessed at.

## Reproduce

```powershell
docker compose -f deploy/docker-compose.yml up -d redis
.\scripts\start-loadtest-env.ps1
k6 run scripts\loadtest.js --summary-export=k6-summary.json
.\scripts\stop-loadtest-env.ps1
```
