# Load test results

Every number below is from `k6-summary.json` plus live console output during
the exact run, produced by the exact commands in [Reproduce](#reproduce) —
nothing here is hand-typed. Run against `scripts/loadtest/providers.yaml`'s
two mock providers (never a real, billable API), on the developer's Windows
machine (see [Known gaps](#known-gaps) for what that does and doesn't prove).

## Summary

| | |
|---|---|
| Total requests through the gateway | 5,400 |
| Duration | ~90s at a fixed 60 req/s arrival rate |
| Gateway overhead, p50 / p90 / p95 | 1.57ms / 2.52ms / 2.84ms |
| Gateway overhead, max | 119.16ms (see [below](#gateway-overhead)) |
| End-to-end latency, p50 / p95 | 25.21ms / 37.41ms |
| Checks passed | 5,400 / 5,400 (100%) — every response was a status the gateway is meant to produce |
| Goroutines, baseline → peak → settled | 19 → 28 → 19 (see [below](#goroutines)) |

## Status code distribution

| Status | Count | Share | Meaning |
|---|---|---|---|
| 200 | 2,923 | 54.1% | Served, directly or via fallback |
| 429 | 1,594 | 29.5% | `loadtest-batch`'s deliberately tight RPM/TPM being enforced under real concurrency |
| 402 | 530 | 9.8% | `loadtest-budget-capped`'s $0.02 cap being enforced |
| 502 / 503 | 353 | 6.5% | `loadtest-budget-capped` requests during the primary-outage window — that team's only allowed model has no fallback tier, so a down primary is a hard failure for it by design (allowlist beats availability, Phase 6) |

Of the 2,923 successes, **1,118 (38.3%)** carried `X-Switchyard-Fallback: true`,
served by `mock-fallback` — these landed inside the run's 30s–60s window,
during which `scripts/loadtest.js`'s `knockPrimaryDown` scenario held
`mock-primary` down. The other two teams' models both sit in the `fast`
fallback tier, so their traffic kept succeeding through the outage; only
the budget-capped team's single-candidate model was actually exposed to it.

This run does not re-prove rate-limit *exactness* under concurrency — that's
the job of `TestRateLimitIsExactUnderConcurrency` in `test/ratelimit_test.go`
(and `TestConsumeConcurrentIsExact` / `TestConcurrentRequestsThroughHTTPRespectSharedRPMBucket`,
which assert an exact admitted count against 200 concurrent clients). What
this run adds is the same enforcement holding up under real, sustained,
mixed-team concurrency over 90 seconds rather than one isolated burst.

## Gateway overhead

`gateway_overhead_ms` is read from `X-Switchyard-Overhead-Ms` on every
response — measured inside the gateway itself, excluding provider time, per
Step 1.6. p50/p90/p95 (1.57 / 2.52 / 2.84ms) are comfortably under the
<10ms target, and k6's own live threshold check confirms it: `✓ 'p(95)<10'
p(95)=2.84ms`.

The 119.16ms max is not noise: a request that retries against a down
`mock-primary` (3 attempts, full-jitter backoff) before falling over to
`mock-fallback` spends that entire retry-and-backoff window inside the
handler, which is legitimately gateway overhead by the Step 1.6 definition —
it is not provider time, and the fallback candidate's own latency is what
gets subtracted, not the failed attempts against the primary. The outlier is
concentrated in the 30s–60s primary-outage window for exactly this reason.

One reporting quirk worth recording rather than hiding: `k6-summary.json`
writes `"gateway_overhead_ms": {"thresholds": {"p(95)<10": false}}` even
though the same run's live terminal output explicitly printed the threshold
as passing (`✓`) with the same p(95) value. Reproduced across two separate
runs with different data — this is a `--summary-export` quirk in k6 v2.2.0,
not a real threshold breach. Trust the live `k6 run` output over the
exported JSON's `thresholds` field for this metric.

## Goroutines

`scripts/start-loadtest-env.ps1` prints `go_goroutines` (from `/metrics`)
once the gateway is healthy and traffic hasn't started; `stop-loadtest-env.ps1`
prints it again right before shutdown. This run:

| When | Count |
|---|---|
| Baseline, before any traffic | 19 |
| Immediately after the 90s run | 27 |
| +20s | 28 (peak) |
| +60s after the run ended | 19 |

The count climbs during the run and for a short window after — expected,
since Go's `net/http` transport keeps idle keep-alive connections (and their
read-loop goroutines) open for a while rather than tearing them down
immediately. It returns to exactly the pre-traffic baseline once those idle
connections time out, with nothing left outstanding. No leak.

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

## Reproduce

```powershell
docker compose -f deploy/docker-compose.yml up -d redis
.\scripts\start-loadtest-env.ps1
k6 run scripts\loadtest.js --summary-export=k6-summary.json
.\scripts\stop-loadtest-env.ps1
```
