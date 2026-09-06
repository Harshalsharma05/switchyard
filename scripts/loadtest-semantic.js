// Semantic-cache mini-run. Small, low-rate, and separate from the main load
// test because every exact-tier miss here pays one real Gemini embedding round
// trip. It answers three questions the main run cannot: does semantically
// similar phrasing hit at the configured threshold, what does the embedding
// call cost, and does gateway overhead stay under budget with the semantic
// tier live (embedding time is excluded from the overhead header, so it should).
//
// Bring the env up with:  .\scripts\start-loadtest-env.ps1 -Semantic
// Run with:               k6 run scripts/loadtest-semantic.js

import http from 'k6/http';
import { check } from 'k6';
import { Trend, Counter, Rate } from 'k6/metrics';

const BASE_URL = __ENV.SWITCHYARD_BASE_URL || 'http://localhost:8080';
const KEY = 'sk-loadtest-realtime-9f2b1c';

http.setResponseCallback(http.expectedStatuses(200));

const overheadMs = new Trend('gateway_overhead_ms', true);
const embedMs = new Trend('embed_ms', true);
const semanticHitRate = new Rate('semantic_hit_rate');
const tierExact = new Counter('tier_exact');
const tierSemantic = new Counter('tier_semantic');
const tierMiss = new Counter('tier_miss');

// Each family shares one meaning; paraphrases should clear the 0.93 cosine
// threshold against the canonical once it is cached.
const FAMILIES = [
  {
    canonical: 'What is the capital city of Japan?',
    paraphrases: ['Which city is the capital of Japan?', "Tell me Japan's capital city.", 'What city serves as the capital of Japan?'],
  },
  {
    canonical: 'Explain how a token bucket rate limiter works.',
    paraphrases: ['How does a token bucket rate limiter function?', 'Describe the mechanism of a token bucket rate limiter.', 'Walk through how token bucket rate limiting works.'],
  },
  {
    canonical: 'What are the benefits of using a message queue?',
    paraphrases: ['Why would you use a message queue?', 'What advantages does a message queue provide?', 'List the upsides of adopting a message queue.'],
  },
  {
    canonical: 'How does TLS protect data in transit?',
    paraphrases: ['In what way does TLS secure data while it travels?', 'Explain how TLS keeps in-transit data safe.', 'How does TLS safeguard data on the wire?'],
  },
  {
    canonical: 'What is the difference between a process and a thread?',
    paraphrases: ['How do processes and threads differ?', 'Compare a process and a thread.', 'What distinguishes a thread from a process?'],
  },
  {
    canonical: 'Describe the purpose of a database index.',
    paraphrases: ['What is a database index for?', 'Why do databases use indexes?', 'Explain what a database index accomplishes.'],
  },
];

// Genuinely different requests — these must stay misses.
const UNRELATED = [
  'Write a haiku about winter mountains.',
  'What time zone is Reykjavik in?',
  'Suggest a name for a pet tortoise.',
  'How many moons does Mars have?',
  'Give me a recipe for a three-egg omelette.',
  'What is the boiling point of water at sea level?',
];

function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}

// store true (setup only) lets the response be cached; false sends
// X-Switchyard-Cache-TTL: 0 so only the six canonicals ever populate the cache
// and the tier breakdown stays a clean paraphrase-vs-miss signal.
function ask(content, store) {
  const headers = { 'Content-Type': 'application/json', Authorization: `Bearer ${KEY}` };
  if (!store) headers['X-Switchyard-Cache-TTL'] = '0';

  const res = http.post(`${BASE_URL}/v1/chat/completions`, JSON.stringify({
    model: 'mock-fast',
    messages: [{ role: 'user', content }],
    stream: false,
    max_tokens: 32,
  }), { headers });

  check(res, { 'status 200': (r) => r.status === 200 });

  const oh = res.headers['X-Switchyard-Overhead-Ms'];
  if (oh) overheadMs.add(parseFloat(oh));
  const em = res.headers['X-Switchyard-Embed-Ms'];
  if (em) embedMs.add(parseFloat(em));

  return res.headers['X-Switchyard-Cache'];
}

export const options = {
  summaryTrendStats: ['min', 'avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max', 'count'],
  scenarios: {
    semanticWorkload: {
      executor: 'constant-arrival-rate',
      rate: 5,
      timeUnit: '1s',
      duration: '60s',
      preAllocatedVUs: 20,
      maxVUs: 40,
    },
  },
  thresholds: {
    gateway_overhead_ms: ['p(95)<10'],
  },
};

// Prime the cache with every canonical before the paraphrase traffic starts.
export function setup() {
  for (const f of FAMILIES) ask(f.canonical, true);
}

export default function () {
  // 65% paraphrases (expect a semantic hit), 35% unrelated (expect a miss).
  const paraphrase = Math.random() < 0.65;
  const content = paraphrase ? pick(pick(FAMILIES).paraphrases) : pick(UNRELATED);
  const tier = ask(content, false);

  if (tier === 'exact') tierExact.add(1);
  else if (tier === 'semantic') tierSemantic.add(1);
  else tierMiss.add(1);

  // Only paraphrase traffic counts toward the hit rate; unrelated prompts are
  // supposed to miss and would just drag the number down.
  if (paraphrase) semanticHitRate.add(tier === 'semantic' || tier === 'exact');
}

export function handleSummary(data) {
  const m = data.metrics;
  const oh = m.gateway_overhead_ms.values;
  const em = m.embed_ms ? m.embed_ms.values : {};

  const summary = {
    total_requests: m.http_reqs.values.count,
    gateway_overhead_ms: { 'p(50)': oh['med'], 'p(95)': oh['p(95)'], 'p(99)': oh['p(99)'], max: oh['max'] },
    overhead_p95_under_10ms: oh['p(95)'] < 10,
    embed_ms: { 'p(50)': em['med'], 'p(95)': em['p(95)'], 'p(99)': em['p(99)'], max: em['max'] },
    semantic_hit_rate: m.semantic_hit_rate ? m.semantic_hit_rate.values.rate : null,
    tier_exact: m.tier_exact ? m.tier_exact.values.count : 0,
    tier_semantic: m.tier_semantic ? m.tier_semantic.values.count : 0,
    tier_miss: m.tier_miss ? m.tier_miss.values.count : 0,
  };

  const f = (v) => (v === undefined || v === null ? '-' : v.toFixed(2));
  const text = [
    `total requests:      ${summary.total_requests}`,
    `overhead ms:         p50=${f(oh['med'])} p95=${f(oh['p(95)'])} p99=${f(oh['p(99)'])}`,
    `overhead p95 < 10ms: ${summary.overhead_p95_under_10ms ? 'PASS' : 'FAIL'}`,
    `embed ms:            p50=${f(em['med'])} p95=${f(em['p(95)'])} p99=${f(em['p(99)'])} max=${f(em['max'])}`,
    `semantic hit rate:   ${summary.semantic_hit_rate === null ? 'n/a' : (summary.semantic_hit_rate * 100).toFixed(1) + '%'}`,
    `tiers:               exact=${summary.tier_exact} semantic=${summary.tier_semantic} miss=${summary.tier_miss}`,
  ].join('\n');

  return {
    'k6-semantic-summary.json': JSON.stringify(summary, null, 2),
    stdout: '\n' + text + '\n',
  };
}
