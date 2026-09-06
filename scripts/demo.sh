#!/usr/bin/env bash
# Part 2, Step 10.3 — the narrated end-to-end demo. Ten scenes: live Overview,
# a streaming Playground request, a cache hit, cost-aware routing, the failure
# chain reaction, recovery, a rate-limit wall, a budget wall, Request Logs with
# a Jaeger deep link, and Usage & Cost. Each API-side action is done here so it
# is deterministic; the parallel "do this in the UI" narration is printed
# alongside. Pauses before every scene so it can be narrated live.
#
# Needs the full stack: `docker compose -f deploy/docker-compose.yml up -d`
# brings up the gateway (chaos on), Redis, Postgres, Prometheus, Jaeger,
# Grafana, and the console. The preflight below fails fast if a piece is
# missing.
#
# Run: bash scripts/demo.sh

set -euo pipefail

BASE_URL="${SWITCHYARD_BASE_URL:-http://localhost:8080}"
ADMIN_URL="${SWITCHYARD_ADMIN_URL:-http://localhost:9090}"
UI_URL="${SWITCHYARD_UI_URL:-http://localhost:3001}"
JAEGER_URL="${SWITCHYARD_JAEGER_URL:-http://localhost:16686}"
GRAFANA_URL="${SWITCHYARD_GRAFANA_URL:-http://localhost:3000}"

# configs/teams.yaml dev keys. acme is admin (reads every team's rows); globex
# is non-admin, rpm 10, budget $5.
ACME_KEY="sk-switchyard-dev-acme-9f2b1c"
GLOBEX_KEY="sk-switchyard-dev-globex-7a4e0d"
GLOBEX_BUDGET_USD="5.00"

CACHE_PROMPT="Explain what a write-ahead log is, in two sentences."
COMPLEX_PROMPT="Analyze the tradeoffs between optimistic and pessimistic locking; you must cover throughput and must not assume a single-node database."
SIMPLE_PROMPT="What is the capital of France?"

have_jq=false
if command -v jq >/dev/null 2>&1; then have_jq=true; fi
pp() { if $have_jq; then jq . 2>/dev/null || cat; else cat; fi; }

scene() {
  echo
  echo "════════════════════════════════════════════════════════════════"
  echo "  $1"
  echo "════════════════════════════════════════════════════════════════"
}
ui() { echo; echo "  IN THE UI ▸ $1"; }
pause() { read -r -p "-- press Enter to continue -- "; }

# chat KEY MODEL CONTENT [EXTRA_JSON] — non-streaming, returns headers + body.
chat() {
  local key="$1" model="$2" content="$3" extra="${4:-}"
  curl -si "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $key" -H "Content-Type: application/json" \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$content\"}]${extra:+,$extra}}"
}

# sy_headers filters a raw response to the status line and the X-Switchyard-*
# headers that matter to the narration. The `|| true` keeps a response with
# none of them (a bare 503, say) from tripping `set -e` via `pipefail`.
sy_headers() {
  grep -iE '^(HTTP/|X-Switchyard-(Provider|Served-Model|Requested-Model|Overhead-Ms|Embed-Ms|Fallback|Cache|Route-Tier|Route-Reason))' | tr -d '\r' | sed 's/^/    /' || true
}

# --- background traffic ------------------------------------------------
# A gentle trickle as acme so Overview, Request Logs and Usage & Cost have
# something live to show: a unique routed prompt (records a routing downgrade)
# then a repeated one (an exact cache hit after warmup).
bg_pid=""
start_background_traffic() {
  (
    i=0
    pool=("What is a bloom filter used for?" "Define eventual consistency." "What is a reverse proxy?" "What does TTL mean in caching?")
    while true; do
      i=$((i + 1))
      curl -s -o /dev/null "$BASE_URL/v1/chat/completions" \
        -H "Authorization: Bearer $ACME_KEY" -H "Content-Type: application/json" \
        -H "X-Switchyard-Cache-TTL: 0" \
        -d "{\"model\":\"auto\",\"messages\":[{\"role\":\"user\",\"content\":\"Define widget number $i.\"}]}" || true
      sleep 3
      curl -s -o /dev/null "$BASE_URL/v1/chat/completions" \
        -H "Authorization: Bearer $ACME_KEY" -H "Content-Type: application/json" \
        -d "{\"model\":\"openai/gpt-oss-20b\",\"messages\":[{\"role\":\"user\",\"content\":\"${pool[$((i % 4))]}\"}]}" || true
      sleep 3
    done
  ) &
  bg_pid=$!
}

cleanup() {
  echo
  echo "Restoring the gateway to a clean state..."
  [ -n "$bg_pid" ] && kill "$bg_pid" >/dev/null 2>&1 || true
  curl -s -X DELETE "$ADMIN_URL/admin/chaos" >/dev/null 2>&1 || true
  curl -s -X PATCH "$ADMIN_URL/admin/teams/globex" \
    -H "Content-Type: application/json" \
    -d "{\"monthly_budget_usd\": $GLOBEX_BUDGET_USD}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preflight -------------------------------------------------------------
scene "Preflight"

if ! curl -sf "$BASE_URL/healthz" >/dev/null; then
  echo "Gateway not reachable at $BASE_URL. Bring the stack up first:"
  echo "  docker compose -f deploy/docker-compose.yml up -d"
  exit 1
fi
echo "Gateway is up: $BASE_URL (admin $ADMIN_URL)"

if [ "$(curl -s -o /dev/null -w '%{http_code}' "$ADMIN_URL/admin/chaos")" != "200" ]; then
  echo "Scenes 5-6 need the chaos harness. Start the gateway with"
  echo "SWITCHYARD_ENV=dev and SWITCHYARD_CHAOS_ENABLED=true (the compose file sets both)."
  exit 1
fi
echo "Chaos harness available."

if [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ACME_KEY" "$ADMIN_URL/admin/requests?limit=1")" != "200" ]; then
  echo "Request Logs not available — Postgres is not wired (POSTGRES_PASSWORD unset)."
  echo "Scenes 9-10 need it. Use the compose stack, not a bare 'go run ./cmd/gateway'."
  exit 1
fi
echo "Request log available."

me=$(curl -s -H "Authorization: Bearer $ACME_KEY" "$ADMIN_URL/admin/me")
echo "$me" | grep -q '"routing"' && echo "Cost-aware routing enabled." || echo "NOTE: routing may be off — scene 4 will be thin."

if ! curl -sf "$UI_URL" >/dev/null 2>&1; then
  echo "NOTE: console not reachable at $UI_URL — the UI narration steps will not have a page to show."
fi

health_json=$(curl -s "$ADMIN_URL/admin/providers/health")
if echo "$health_json" | grep -q '"status":"down"\|"status":"degraded"'; then
  echo
  echo "WARNING: a provider is already degraded/down. Scene 5's failover still"
  echo "works but reads messier than a clean single hop. For a clean recording,"
  echo "check Ollama is up (ollama pull llama3.2:3b) and both API keys are valid."
fi

start_background_traffic
echo
echo "Background traffic started (acme, a routed prompt + a cached prompt every ~6s)."
pause

# --- 1. Overview with live traffic ---------------------------------------
scene "1 · Overview, live"
echo "Traffic is flowing. A couple of the requests just logged:"
curl -s -H "Authorization: Bearer $ACME_KEY" "$ADMIN_URL/admin/requests?team=acme&limit=3" | pp
ui "Open $UI_URL → Overview. KPI row (requests, overhead p95, error rate, cache"
ui "hit rate, cost) updating without a refresh; the traffic and overhead charts"
ui "moving; the provider-health strip and breaker states live; the request feed"
ui "scrolling. Set the range to 1h. 'Open in Grafana ↗' opens $GRAFANA_URL."
pause

# --- 2. A streaming Playground request ----------------------------------
scene "2 · A streaming request, with metadata"
cache_body="{\"model\":\"openai/gpt-oss-20b\",\"messages\":[{\"role\":\"user\",\"content\":\"$CACHE_PROMPT\"}]"
echo "Streaming request as acme — this is the cache MISS: it embeds the prompt"
echo "and calls the provider."
miss=$(curl -si -N -w '\n  time_total=%{time_total}s\n' "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $ACME_KEY" -H "Content-Type: application/json" \
  -d "$cache_body,\"stream\":true}")
echo
printf '%s\n' "$miss" | sy_headers
echo "  first SSE frames:"
printf '%s\n' "$miss" | grep '^data:' | head -n 3 | sed 's/^/    /' || true
printf '%s\n' "$miss" | grep time_total || true
ui "Open Playground, streaming ON, model openai/gpt-oss-20b. Paste:"
ui "  \"$CACHE_PROMPT\""
ui "It renders token by token. Metadata panel: provider, served model, gateway"
ui "overhead, embedding time, tokens in/out, cost, Cache = miss."
pause

# --- 3. The same prompt again — cache hit -------------------------------
scene "3 · Send it again — cache hit"
echo "The identical request, once more:"
hit=$(curl -si -w '\n  time_total=%{time_total}s\n' "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $ACME_KEY" -H "Content-Type: application/json" \
  -d "$cache_body}")
echo
printf '%s\n' "$hit" | sy_headers
printf '%s\n' "$hit" | grep time_total || true
echo
echo "Compare the two time_total values: the miss paid an embedding call and a"
echo "provider round trip; this one is a single Redis read."
ui "Send the exact same prompt in Playground again. It returns instantly."
ui "Metadata panel: Cache = exact hit, cost \$0, served by the original provider."
pause

# --- 4. Cost-aware routing --------------------------------------------------
scene "4 · Routing — complex up, simple down"
route() {
  curl -si "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $ACME_KEY" -H "Content-Type: application/json" \
    -H "X-Switchyard-Cache-TTL: 0" \
    -d "{\"model\":\"auto\",\"messages\":[{\"role\":\"user\",\"content\":\"$1\"}]}" | sy_headers
}
echo "model:\"auto\", a complex prompt (Cache-TTL:0 so routing runs every time):"
route "$COMPLEX_PROMPT"
echo
echo "model:\"auto\", a simple prompt:"
route "$SIMPLE_PROMPT"
ui "Playground, model = auto. The complex prompt routes to the frontier tier"
ui "(gpt-oss-120b); the simple one routes to fast (gpt-oss-20b). The metadata"
ui "panel shows the tier and X-Switchyard-Route-Reason — the classifier's math."
pause

# --- 5. Failure simulation — the chain reaction ------------------------
scene "5 · Fail groq — the chain reaction"
echo "Forcing groq/openai/gpt-oss-20b into synthetic errors:"
curl -s -X POST "$ADMIN_URL/admin/chaos" -H "Content-Type: application/json" \
  -d '{"rules":[{"provider":"groq","model":"openai/gpt-oss-20b","mode":"error"}]}' | pp
echo
echo "Six requests as acme (allowed to fall back to Gemini):"
for i in $(seq 1 6); do
  echo "  attempt $i:"
  chat "$ACME_KEY" "openai/gpt-oss-20b" "hi" | sy_headers
done
echo
echo "Provider health now:"
curl -s "$ADMIN_URL/admin/providers/health" | pp
ui "Live Ops. In real time: groq's status flips healthy → degraded → down, its"
ui "breaker closed → open, and the request feed shows fallback to gemini. The"
ui "chaos control on this screen does exactly what the POST above just did."
pause

# --- 6. Recovery ---------------------------------------------------------
scene "6 · Recovery"
echo "Clearing the fault:"
curl -s -X DELETE "$ADMIN_URL/admin/chaos" | pp
echo
echo "Waiting out the breaker cooldown (~11s)..."
sleep 11
echo "Half-open probe:"
chat "$ACME_KEY" "openai/gpt-oss-20b" "hi" | sy_headers
echo
echo "And again — normal routing resumed, no fallback header true:"
chat "$ACME_KEY" "openai/gpt-oss-20b" "hi" | sy_headers
ui "Live Ops: breaker half-open (one probe) → closed, health climbs back to"
ui "healthy. The recovery is as visible as the failure was."
pause

# --- 7. Rate limit -------------------------------------------------------
scene "7 · Hitting the rate limit"
echo "globex's RPM cap is 10. Twenty requests back to back:"
statuses=""
for i in $(seq 1 20); do
  s=$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $GLOBEX_KEY" -H "Content-Type: application/json" \
    -d '{"model":"openai/gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}')
  statuses="$statuses $s"
done
echo "$statuses" | tr ' ' '\n' | grep -v '^$' | sort | uniq -c
echo
echo "The 429 body carries Retry-After and the X-RateLimit-* headers:"
curl -si "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $GLOBEX_KEY" -H "Content-Type: application/json" \
  -d '{"model":"openai/gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}' \
  | grep -iE '^(HTTP/|Retry-After|X-RateLimit-)' | tr -d '\r' | sed 's/^/    /' || true
ui "Live Ops → load simulator (sign in as globex first). Concurrency 5, 15s."
ui "RPS, the 429 count and p50/p95/p99 update live. The screen states these are"
ui "indicative numbers — the benchmark figures come from scripts/loadtest.js."
pause

# --- 8. Budget wall ----------------------------------------------------
scene "8 · Budget exhausted → 402"
echo "Capping globex's monthly budget to \$0.00001 (the same PATCH an operator"
echo "would use live):"
curl -s -X PATCH "$ADMIN_URL/admin/teams/globex" -H "Content-Type: application/json" \
  -d '{"monthly_budget_usd": 0.00001}' | pp
echo
echo "One request as globex — denied before any provider is called:"
chat "$GLOBEX_KEY" "openai/gpt-oss-20b" "hi"
echo
echo "Restoring globex to \$$GLOBEX_BUDGET_USD:"
curl -s -X PATCH "$ADMIN_URL/admin/teams/globex" -H "Content-Type: application/json" \
  -d "{\"monthly_budget_usd\": $GLOBEX_BUDGET_USD}" | pp
ui "The 402 body reads '\$X of \$Y'. Usage & Cost shows globex's budget bar at"
ui "100% — the blocked state, not just the 80% warning."
pause

# --- 9. Request Logs + Jaeger ---------------------------------------------
scene "9 · Request Logs → Jaeger"
echo "The failures from scene 5, from the request log:"
rows=$(curl -s -H "Authorization: Bearer $ACME_KEY" "$ADMIN_URL/admin/requests?provider=groq&fallback=true&limit=5")
echo "$rows" | pp
if $have_jq; then
  trace=$(echo "$rows" | jq -r '.requests[0].trace_id // empty')
else
  trace=$(echo "$rows" | grep -oE '"trace_id":"[a-fA-F0-9]+"' | head -1 | cut -d'"' -f4 || true)
fi
if [ -n "$trace" ]; then
  echo
  echo "Deep link to that exact request's trace:"
  echo "  $JAEGER_URL/trace/$trace"
fi
ui "Request Logs. Filter provider = groq, fallback = true (or status = 5xx)."
ui "Open a row — the drawer shows every stored field, the routing decision, the"
ui "fallback chain, and 'View trace in Jaeger' → the full distributed trace."
pause

# --- 10. Usage & Cost ---------------------------------------------------
scene "10 · Usage & Cost"
echo "Attribution over the last 24h:"
curl -s -H "Authorization: Bearer $ACME_KEY" "$ADMIN_URL/admin/attribution?range=24h" | pp
echo
echo "Redis budget counters vs the request-log sum:"
curl -s -H "Authorization: Bearer $ACME_KEY" "$ADMIN_URL/admin/reconciliation" | pp
ui "Usage & Cost. Cost saved by cache and cost saved by routing, each its own"
ui "number; the cost the fallback shifted; per-team spend bars; and the"
ui "reconciliation strip showing Redis and the request log agree."

scene "Done"
echo "Ten scenes complete. Cleanup (next) clears chaos, restores globex's"
echo "budget, and stops the background traffic."
