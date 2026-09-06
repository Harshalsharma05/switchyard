// k6 load test for the SwitchYard gateway, Part 2. Keeps Part 1's mixed
// workload — three teams, mid-run primary outage, one team over its rate limit
// and one over its budget — and layers two Part 2 slices onto the realtime
// team's traffic: a cache-repeat slice (exact-tier hits) and a routing slice
// (model: "auto", varied complexity).
//
// Only the cache slice is cacheable. Every other request sends
// X-Switchyard-Cache-TTL: 0, so it still pays the exact-tier lookup on the hot
// path but is never served from cache — otherwise the exact cache absorbs the
// whole workload and the rate-limit, budget and failover numbers stop being
// comparable to Part 1 (k6's __ITER is per-VU, so "unique" prompts repeat).
//
// Run with: k6 run scripts/loadtest.js

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter, Rate } from 'k6/metrics';

const BASE_URL = __ENV.SWITCHYARD_BASE_URL || 'http://localhost:8080';
const PRIMARY_CONTROL_URL = __ENV.MOCK_PRIMARY_CONTROL_URL || 'http://127.0.0.1:9501/__control/state';

// Only genuine gateway failures move http_req_failed. The run deliberately
// produces 429/402/503, and 502 in the primary-outage window.
http.setResponseCallback(http.expectedStatuses(200, 402, 429, 502, 503));

// From X-Switchyard-Overhead-Ms — measured inside the gateway, provider time
// and embedding time excluded.
const gatewayOverheadMs = new Trend('gateway_overhead_ms', true);
const rateLimited429 = new Counter('rate_limited_429');
const budgetDenied402 = new Counter('budget_denied_402');
const serverError5xx = new Counter('server_error_5xx');
const fallbackServed = new Counter('fallback_served');
const unexpectedStatus = new Counter('unexpected_status');

// Part 2 slices.
const cacheHitRate = new Rate('cache_hit_rate');
const cacheHitLatency = new Trend('cache_hit_latency_ms', true);
const cacheMissLatency = new Trend('cache_miss_latency_ms', true);
const routedToFast = new Counter('routed_to_fast');
const routedToFrontier = new Counter('routed_to_frontier');

const KEYS = {
  realtime: 'sk-loadtest-realtime-9f2b1c',
  batch: 'sk-loadtest-batch-7a4e0d',
  budget: 'sk-loadtest-budgetcapped-3c1d8f',
};

const ACCEPTED_STATUSES = [200, 429, 402, 502, 503];

// A small fixed pool, sent verbatim: the second and later send of each is an
// exact-tier hit (same team + model + messages + max_tokens is the same key).
// Deliberately small — this measures the exact tier's mechanics and cost, not
// a realistic production hit rate.
const CACHE_PROMPTS = [
  'Summarize the CAP theorem in two sentences.', 'What does the acronym SLA stand for?',
  'List three idempotent HTTP methods.', 'Give a one-line definition of eventual consistency.',
  'What is a bloom filter used for?', 'State the difference between latency and throughput.',
  'What is a write-ahead log?', 'Define backpressure in a streaming system.',
  'What is a monotonic clock?', 'Explain what a dead letter queue is.',
  'What does idempotent mean for an HTTP method?', 'Define a race condition in one sentence.',
  'What is the purpose of a health check endpoint?', 'What is connection pooling?',
  'Give a one-line definition of a circuit breaker.', 'What is a p99 latency?',
  'What does TTL stand for in caching?', 'Define horizontal scaling.',
  'What is a reverse proxy?', 'Explain what a semaphore is in one sentence.',
  'What is exponential backoff?', 'Define a cache stampede.',
  'What is a bearer token?', 'What is the thundering herd problem?',
];

// model: "auto" — the lexical classifier sends these to the cheap `fast` tier.
const SIMPLE_PROMPTS = [
  'What is the capital of Australia?', 'Define idempotence.',
  'Convert 100 kilometres to miles.', 'Who wrote the original Unix shell?',
];
// ...and these, dense with reasoning verbs and constraints, to `frontier`.
const COMPLEX_PROMPTS = [
  'Analyze the trade-offs between optimistic and pessimistic locking; you must cover throughput and must not assume a single-node database.',
  'Design a rate limiter for a distributed API. Compare token bucket and sliding window and explain why you would pick one.',
  'Debug why a Go service leaks goroutines under load and walk me through the root cause step by step.',
  'Derive the expected number of probes for an open-addressing hash table at load factor 0.75 and prove your bound.',
];

function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}

export const options = {
  summaryTrendStats: ['min', 'avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
  scenarios: {
    mixedWorkload: {
      executor: 'constant-arrival-rate',
      rate: 60,
      timeUnit: '1s',
      duration: '90s',
      preAllocatedVUs: 50,
      maxVUs: 200,
      exec: 'mixedWorkload',
    },
    knockPrimaryDown: {
      executor: 'shared-iterations', vus: 1, iterations: 1, startTime: '30s', exec: 'knockPrimaryDown',
    },
    restorePrimary: {
      executor: 'shared-iterations', vus: 1, iterations: 1, startTime: '60s', exec: 'restorePrimary',
    },
  },
  thresholds: {
    gateway_overhead_ms: ['p(95)<10'],
  },
};

// send posts one completion and records the metrics every slice shares.
// cacheable false attaches X-Switchyard-Cache-TTL: 0 — the request is still
// looked up in the exact tier but is never stored or served from cache.
function send(key, body, tag, cacheable) {
  const headers = { 'Content-Type': 'application/json', Authorization: `Bearer ${key}` };
  if (!cacheable) headers['X-Switchyard-Cache-TTL'] = '0';

  const res = http.post(`${BASE_URL}/v1/chat/completions`, JSON.stringify(body), {
    headers,
    tags: { slice: tag, model: body.model, stream: String(!!body.stream) },
  });

  const overhead = res.headers['X-Switchyard-Overhead-Ms'];
  if (overhead) gatewayOverheadMs.add(parseFloat(overhead));

  if (res.status === 429) rateLimited429.add(1);
  else if (res.status === 402) budgetDenied402.add(1);
  else if (res.status === 502 || res.status === 503) serverError5xx.add(1);
  else if (res.headers['X-Switchyard-Fallback'] === 'true') fallbackServed.add(1);

  const ok = check(res, {
    'status is one of 200, 429, 402, 502, 503': (r) => ACCEPTED_STATUSES.includes(r.status),
  });
  if (!ok) unexpectedStatus.add(1);

  return res;
}

// Part 1 team weighting, unchanged: realtime 60%, batch 30%, budget 10%.
export function mixedWorkload() {
  const r = Math.random() * 10;
  if (r < 6) realtimeRequest();
  else if (r < 9) batchRequest();
  else budgetRequest();
}

// (__VU, __ITER) is unique within a run — real per-request prompts, not the
// ~100-value pool a bare __ITER produces under constant-arrival-rate.
function uniquePrompt() {
  return `load test ${__VU}_${__ITER}`;
}

function realtimeRequest() {
  const roll = Math.random();
  if (roll < 0.25) return cacheRequest();
  if (roll < 0.45) return routedRequest();

  send(KEYS.realtime, {
    model: pick(['mock-fast', 'mock-fast-b', 'mock-frontier']),
    messages: [{ role: 'user', content: uniquePrompt() }],
    stream: Math.random() < 0.2,
    max_tokens: 32,
  }, 'baseline', false);
}

function batchRequest() {
  send(KEYS.batch, {
    model: pick(['mock-fast', 'mock-fast-b']),
    messages: [{ role: 'user', content: uniquePrompt() }],
    stream: Math.random() < 0.2,
    max_tokens: 32,
  }, 'baseline', false);
}

function budgetRequest() {
  send(KEYS.budget, {
    model: 'mock-frontier',
    messages: [{ role: 'user', content: uniquePrompt() }],
    stream: Math.random() < 0.2,
    max_tokens: 32,
  }, 'baseline', false);
}

function cacheRequest() {
  const res = send(KEYS.realtime, {
    model: 'mock-fast',
    messages: [{ role: 'user', content: pick(CACHE_PROMPTS) }],
    stream: false,
    max_tokens: 32,
  }, 'cache', true);

  if (res.status !== 200) return;
  const tier = res.headers['X-Switchyard-Cache'];
  const hit = tier === 'exact' || tier === 'semantic';
  cacheHitRate.add(hit);
  (hit ? cacheHitLatency : cacheMissLatency).add(res.timings.duration);
}

function routedRequest() {
  const complex = Math.random() < 0.5;
  const res = send(KEYS.realtime, {
    model: 'auto',
    messages: [{ role: 'user', content: complex ? pick(COMPLEX_PROMPTS) : pick(SIMPLE_PROMPTS) }],
    stream: false,
    max_tokens: 32,
  }, 'routed', false);

  const tier = res.headers['X-Switchyard-Route-Tier'];
  if (tier === 'fast') routedToFast.add(1);
  else if (tier === 'frontier') routedToFrontier.add(1);
}

export function knockPrimaryDown() {
  http.post(PRIMARY_CONTROL_URL, JSON.stringify({ down: true }), {
    headers: { 'Content-Type': 'application/json' },
  });
}

export function restorePrimary() {
  http.post(PRIMARY_CONTROL_URL, JSON.stringify({ down: false }), {
    headers: { 'Content-Type': 'application/json' },
  });
}

// handleSummary replaces --summary-export. Part 1 hit a k6 v2.2.0 bug where the
// exported JSON wrote "p(95)<10": false even on a run the live output passed;
// here the verdict is computed directly from the trend value.
export function handleSummary(data) {
  const m = data.metrics;
  const oh = m.gateway_overhead_ms.values;
  const e2e = m.http_req_duration.values;
  const p95 = oh['p(95)'];

  const summary = {
    total_requests: m.http_reqs.values.count,
    gateway_overhead_ms: {
      'p(50)': oh['med'], 'p(90)': oh['p(90)'], 'p(95)': p95, 'p(99)': oh['p(99)'], max: oh['max'],
    },
    overhead_p95_under_10ms: p95 < 10,
    e2e_latency_ms: { 'p(50)': e2e['med'], 'p(95)': e2e['p(95)'] },
    http_req_failed_rate: m.http_req_failed.values.rate,
    checks_passed: m.checks.values.passes,
    checks_failed: m.checks.values.fails,
    cache_slice: {
      hit_rate: count(m, 'cache_hit_rate', 'rate'),
      hits: m.cache_hit_latency_ms ? m.cache_hit_latency_ms.values.count : 0,
      misses: m.cache_miss_latency_ms ? m.cache_miss_latency_ms.values.count : 0,
      hit_latency_ms: m.cache_hit_latency_ms ? m.cache_hit_latency_ms.values : null,
      miss_latency_ms: m.cache_miss_latency_ms ? m.cache_miss_latency_ms.values : null,
    },
    routed_to_fast: count(m, 'routed_to_fast', 'count'),
    routed_to_frontier: count(m, 'routed_to_frontier', 'count'),
    rate_limited_429: count(m, 'rate_limited_429', 'count'),
    budget_denied_402: count(m, 'budget_denied_402', 'count'),
    server_error_5xx: count(m, 'server_error_5xx', 'count'),
    fallback_served: count(m, 'fallback_served', 'count'),
    unexpected_status: count(m, 'unexpected_status', 'count'),
  };

  return {
    'k6-summary.json': JSON.stringify(summary, null, 2),
    stdout: '\n' + renderText(summary) + '\n',
  };
}

function count(metrics, name, field) {
  return metrics[name] ? metrics[name].values[field] : null;
}

function fmt(v) {
  return v === undefined || v === null ? '-' : v.toFixed(2);
}

function renderText(s) {
  const oh = s.gateway_overhead_ms;
  const c = s.cache_slice;
  return [
    `total requests:       ${s.total_requests}`,
    `gateway overhead ms:  p50=${fmt(oh['p(50)'])} p90=${fmt(oh['p(90)'])} p95=${fmt(oh['p(95)'])} p99=${fmt(oh['p(99)'])} max=${fmt(oh.max)}`,
    `overhead p95 < 10ms:  ${s.overhead_p95_under_10ms ? 'PASS' : 'FAIL'}`,
    `e2e latency ms:       p50=${fmt(s.e2e_latency_ms['p(50)'])} p95=${fmt(s.e2e_latency_ms['p(95)'])}`,
    `http_req_failed:      ${(s.http_req_failed_rate * 100).toFixed(2)}%  (deliberate 429/402/503/502 excluded)`,
    `checks:               ${s.checks_passed} passed / ${s.checks_failed} failed`,
    `cache slice:          ${c.hits} hits / ${c.misses} misses  (${c.hit_rate === null ? 'n/a' : (c.hit_rate * 100).toFixed(1) + '%'})`,
    `cache hit vs miss:    ${fmt(c.hit_latency_ms && c.hit_latency_ms['med'])}ms  vs  ${fmt(c.miss_latency_ms && c.miss_latency_ms['med'])}ms  (e2e median)`,
    `routed:               fast=${s.routed_to_fast} frontier=${s.routed_to_frontier}`,
    `429=${s.rate_limited_429}  402=${s.budget_denied_402}  5xx=${s.server_error_5xx}  fallback=${s.fallback_served}  unexpected=${s.unexpected_status}`,
  ].join('\n');
}
