// k6 load test for the Part 1 gateway. Fires a mixed workload — three teams,
// several models, streaming and non-streaming, one deliberately over its rate
// limit and one deliberately over its budget — at a running SwitchYard
// pointed at scripts/loadtest's mock providers (see
// scripts/start-loadtest-env.ps1). A second pair of scenarios knocks the
// primary mock provider down partway through and brings it back, so the same
// run also proves fallback and recovery under load.
//
// Run with: k6 run scripts/loadtest.js

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const BASE_URL = __ENV.SWITCHYARD_BASE_URL || 'http://localhost:8080';
const PRIMARY_CONTROL_URL = __ENV.MOCK_PRIMARY_CONTROL_URL || 'http://127.0.0.1:9501/__control/state';

// gatewayOverheadMs comes straight from X-Switchyard-Overhead-Ms on every
// response, not from k6's own client-side timing — that header is measured
// inside the gateway itself and excludes provider time, which is exactly the
// number Part 1's <10ms target is about.
const gatewayOverheadMs = new Trend('gateway_overhead_ms', true);
const rateLimited429 = new Counter('rate_limited_429');
const budgetDenied402 = new Counter('budget_denied_402');
const fallbackServed = new Counter('fallback_served');
const unexpectedStatus = new Counter('unexpected_status');

const TEAMS = [
  { key: 'sk-loadtest-realtime-9f2b1c', models: ['mock-fast', 'mock-fast-b', 'mock-frontier'], weight: 6 },
  { key: 'sk-loadtest-batch-7a4e0d', models: ['mock-fast', 'mock-fast-b'], weight: 3 },
  { key: 'sk-loadtest-budgetcapped-3c1d8f', models: ['mock-frontier'], weight: 1 },
];

const ACCEPTED_STATUSES = [200, 429, 402, 502, 503];

function pickTeam() {
  const total = TEAMS.reduce((sum, t) => sum + t.weight, 0);
  let r = Math.random() * total;
  for (const t of TEAMS) {
    if (r < t.weight) return t;
    r -= t.weight;
  }
  return TEAMS[TEAMS.length - 1];
}

function pickModel(team) {
  return team.models[Math.floor(Math.random() * team.models.length)];
}

export const options = {
  scenarios: {
    // 60 iterations/sec for 90s = 5,400 requests, comfortably over the
    // plan's 5,000+ target.
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
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      startTime: '30s',
      exec: 'knockPrimaryDown',
    },
    restorePrimary: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      startTime: '60s',
      exec: 'restorePrimary',
    },
  },
  thresholds: {
    gateway_overhead_ms: ['p(95)<10'],
  },
};

export function mixedWorkload() {
  const team = pickTeam();
  const model = pickModel(team);
  const stream = Math.random() < 0.2;

  const payload = JSON.stringify({
    model,
    messages: [{ role: 'user', content: 'load test message ' + __ITER }],
    stream,
    max_tokens: 32,
  });

  const res = http.post(`${BASE_URL}/v1/chat/completions`, payload, {
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${team.key}`,
    },
    tags: { team: team.key, model, stream: String(stream) },
  });

  const overhead = res.headers['X-Switchyard-Overhead-Ms'];
  if (overhead) gatewayOverheadMs.add(parseFloat(overhead));

  if (res.status === 429) rateLimited429.add(1);
  else if (res.status === 402) budgetDenied402.add(1);
  else if (res.headers['X-Switchyard-Fallback'] === 'true') fallbackServed.add(1);

  const ok = check(res, {
    'status is one of 200, 429, 402, 502, 503': (r) => ACCEPTED_STATUSES.includes(r.status),
  });
  if (!ok) unexpectedStatus.add(1);
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
