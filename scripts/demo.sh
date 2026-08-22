#!/usr/bin/env bash
# Step 11.4's narrated demo: normal request, hammer the rate limit, kill a
# provider (fallback engages), restore it (breaker closes), push a team over
# budget. Pauses before each scene so it can be narrated live. Assumes the
# gateway is already running per README's Quickstart, started with
# SWITCHYARD_ENV=dev and SWITCHYARD_CHAOS_ENABLED=true (scenes 3-4 need the
# chaos harness; the preflight check below fails fast with the fix if it's
# not on).
#
# Run: bash scripts/demo.sh

set -euo pipefail

BASE_URL="${SWITCHYARD_BASE_URL:-http://localhost:8080}"
ADMIN_URL="${SWITCHYARD_ADMIN_URL:-http://localhost:9090}"

ACME_KEY="sk-switchyard-dev-acme-9f2b1c"
GLOBEX_KEY="sk-switchyard-dev-globex-7a4e0d"
GLOBEX_BUDGET_USD="5.00"

have_jq=false
if command -v jq >/dev/null 2>&1; then have_jq=true; fi

pp() {
  if $have_jq; then jq . 2>/dev/null || cat; else cat; fi
}

scene() {
  echo
  echo "════════════════════════════════════════════════════════════════"
  echo "  $1"
  echo "════════════════════════════════════════════════════════════════"
}

pause() {
  read -r -p "-- press Enter to continue -- "
}

chat() {
  local key="$1" model="$2" content="$3"
  curl -si "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $key" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"$content\"}]}"
}

cleanup() {
  echo
  echo "Restoring the gateway to a clean state..."
  curl -s -X DELETE "$ADMIN_URL/admin/chaos" >/dev/null 2>&1 || true
  curl -s -X PATCH "$ADMIN_URL/admin/teams/globex" \
    -H "Content-Type: application/json" \
    -d "{\"monthly_budget_usd\": $GLOBEX_BUDGET_USD}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- preflight ---------------------------------------------------------
scene "Preflight"

if ! curl -sf "$BASE_URL/healthz" >/dev/null; then
  echo "Gateway not reachable at $BASE_URL."
  echo "Start it first (see README Quickstart):"
  echo "  docker compose -f deploy/docker-compose.yml up -d"
  echo "  go run ./cmd/gateway"
  exit 1
fi
echo "Gateway is up: $BASE_URL (admin: $ADMIN_URL)"

chaos_status=$(curl -s -o /dev/null -w "%{http_code}" "$ADMIN_URL/admin/chaos")
if [ "$chaos_status" != "200" ]; then
  echo
  echo "Scenes 3-4 need the chaos harness, which isn't available on this gateway."
  echo "Restart it with:"
  echo "  SWITCHYARD_ENV=dev SWITCHYARD_CHAOS_ENABLED=true go run ./cmd/gateway"
  exit 1
fi
echo "Chaos harness is available."

health_json=$(curl -s "$ADMIN_URL/admin/providers/health")
if echo "$health_json" | grep -q '"status":"down"\|"status":"degraded"'; then
  echo
  echo "WARNING: not every provider is healthy right now:"
  echo "$health_json" | pp
  echo
  echo "Scene 3's failover will still work, but a pre-existing outage makes it"
  echo "look messier than a clean single-hop fallback (the chain may exhaust"
  echo "past Gemini to Ollama, or all the way to a 503). For a clean recording:"
  echo "  - make sure Ollama is running and has the model: ollama pull llama3.2:3b"
  echo "  - make sure GROQ_API_KEY and GEMINI_API_KEY in .env are both valid"
  echo "Fine to proceed anyway — this is informational, not a hard stop."
fi

pause

# --- 1. normal request ---------------------------------------------------
scene "1 · A normal request succeeds"

echo "POST /v1/chat/completions as acme, model openai/gpt-oss-20b (Groq)..."
echo
chat "$ACME_KEY" "openai/gpt-oss-20b" "Say hello in five words." | pp
echo
echo "Now open Jaeger: http://localhost:16686 — service 'switchyard' — and find"
echo "this trace. switchyard.auth, switchyard.ratelimit, switchyard.budget.check,"
echo "and switchyard.provider.call all nest under one root span."

pause

# --- 2. hammer the rate limit --------------------------------------------
scene "2 · Hammering the rate limit"

echo "globex's RPM cap is 10. Firing 20 requests back to back..."
echo
statuses=""
for i in $(seq 1 20); do
  status=$(curl -s -o /dev/null -w "%{http_code}" "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $GLOBEX_KEY" \
    -H "Content-Type: application/json" \
    -d '{"model":"openai/gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}')
  statuses="$statuses $status"
  printf "  request %2d -> %s\n" "$i" "$status"
done
echo
echo "Status code counts:"
echo "$statuses" | tr ' ' '\n' | grep -v '^$' | sort | uniq -c
echo
echo "A code other than 200 or 429 here means something upstream of the"
echo "limiter (Redis, most likely) isn't reachable — check the gateway's logs."
echo "Open Grafana: http://localhost:3000 — Business dashboard — 'Rate limit"
echo "rejections by team' panel."

pause

# --- 3. kill a provider ---------------------------------------------------
scene "3 · Killing a provider"

echo "Forcing every call to Groq's fast-tier model into a synthetic failure..."
curl -s -X POST "$ADMIN_URL/admin/chaos" \
  -H "Content-Type: application/json" \
  -d '{"rules":[{"provider":"groq","model":"openai/gpt-oss-20b","mode":"error"}]}' | pp
echo
echo "Firing 6 requests as acme (allowed to fall back to Gemini) to cross the"
echo "breaker's failure threshold..."
echo
for i in $(seq 1 6); do
  headers=$(curl -si "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $ACME_KEY" \
    -H "Content-Type: application/json" \
    -d '{"model":"openai/gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}' \
    | grep -iE '^(HTTP|X-Switchyard-(Fallback|Served-Model))' | tr -d '\r')
  echo "  attempt $i:"
  echo "$headers" | sed 's/^/    /'
done
echo
echo "Provider health:"
curl -s "$ADMIN_URL/admin/providers/health" | pp
echo
echo "Open Grafana Operations dashboard — 'Provider health' and 'Circuit"
echo "breaker state' panels should show Groq degraded/down and its breaker open."
echo "'Fallback events' should show the traffic Gemini just picked up."

pause

# --- 4. restore the provider ----------------------------------------------
scene "4 · Restoring the provider"

echo "Clearing the injected fault..."
curl -s -X DELETE "$ADMIN_URL/admin/chaos" | pp
echo
echo "Waiting for the breaker's cooldown to elapse (~11s)..."
sleep 11
echo
echo "Sending the half-open probe..."
chat "$ACME_KEY" "openai/gpt-oss-20b" "hi" | grep -iE '^(HTTP|X-Switchyard-(Fallback|Served-Model))' | tr -d '\r'
echo
echo "One more, to confirm normal routing resumed (no X-Switchyard-Fallback):"
chat "$ACME_KEY" "openai/gpt-oss-20b" "hi" | grep -iE '^(HTTP|X-Switchyard-(Fallback|Served-Model))' | tr -d '\r'
echo
echo "Circuit breaker state on the Operations dashboard should be back to closed."

pause

# --- 5. push a team over budget --------------------------------------------
scene "5 · Pushing a team over budget"

echo "Temporarily capping globex's monthly budget to \$0.00001 via the admin API"
echo "(this is the same PATCH an operator would use to adjust a real cap live)..."
curl -s -X PATCH "$ADMIN_URL/admin/teams/globex" \
  -H "Content-Type: application/json" \
  -d '{"monthly_budget_usd": 0.00001}' | pp
echo
echo "One request as globex — denied before Groq is ever called:"
chat "$GLOBEX_KEY" "openai/gpt-oss-20b" "hi"
echo
echo "Grafana Business dashboard — 'Budget utilization by team' — globex should"
echo "read at or past 100%."
echo
echo "Restoring globex's budget to \$$GLOBEX_BUDGET_USD..."
curl -s -X PATCH "$ADMIN_URL/admin/teams/globex" \
  -H "Content-Type: application/json" \
  -d "{\"monthly_budget_usd\": $GLOBEX_BUDGET_USD}" | pp

scene "Done"
echo "All five scenes complete. The gateway has been returned to normal —"
echo "chaos cleared, globex's budget restored."
