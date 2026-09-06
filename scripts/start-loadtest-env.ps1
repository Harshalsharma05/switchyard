# Builds and starts everything the load runs need: two mock providers
# (scripts/mockprovider) and the real gateway binary pointed at
# scripts/loadtest's config, all as background processes. Brings up Redis and
# Postgres from deploy/docker-compose.yml and applies the request-log schema.
# Run scripts/stop-loadtest-env.ps1 when the load test is done.
#
#   .\scripts\start-loadtest-env.ps1            main run: exact cache tier
#   .\scripts\start-loadtest-env.ps1 -Semantic  semantic tier on (real Gemini)

param(
    [switch]$Semantic
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repoRoot "bin"
$logDir = Join-Path $repoRoot "scripts\loadtest\logs"
$pidFile = Join-Path $env:TEMP "switchyard-loadtest-pids.json"
$composeFile = Join-Path $repoRoot "deploy\docker-compose.yml"

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

# .env holds GROQ_API_KEY (quality judge), GEMINI_API_KEY (semantic embeddings)
# and POSTGRES_PASSWORD (request log). The gateway reads these from the
# environment; PowerShell does not load .env on its own.
$envFile = Join-Path $repoRoot ".env"
if (-not (Test-Path $envFile)) {
    Write-Error "$envFile not found. Copy .env.example to .env and fill in the keys."
    exit 1
}
foreach ($line in Get-Content $envFile) {
    if ($line -match '^\s*#' -or $line -notmatch '=') { continue }
    $name, $value = $line -split '=', 2
    Set-Item -Path "Env:$($name.Trim())" -Value $value.Trim()
}

Write-Host "Bringing up Redis and Postgres..."
& docker compose -f $composeFile up -d --wait redis postgres
if ($LASTEXITCODE -ne 0) { Write-Error "docker compose failed to start redis/postgres"; exit 1 }

Write-Host "Building gateway, migrate, and mock provider binaries..."
& go build -o (Join-Path $binDir "switchyard-gateway.exe") (Join-Path $repoRoot "cmd\gateway")
& go build -o (Join-Path $binDir "switchyard-migrate.exe") (Join-Path $repoRoot "cmd\migrate")
& go build -o (Join-Path $binDir "mockprovider.exe") (Join-Path $repoRoot "scripts\mockprovider")

$env:SWITCHYARD_POSTGRES_HOST = "localhost:5432"

Write-Host "Applying the request-log schema..."
& (Join-Path $binDir "switchyard-migrate.exe")
if ($LASTEXITCODE -ne 0) { Write-Error "migration failed"; exit 1 }

# Every run starts from a clean slate: rate-limit / budget / cache / breaker
# state in Redis and request-log rows in Postgres both persist across runs
# otherwise, so a prior run's spend or cached answers would skew this one's
# numbers (a monthly budget counter especially — it would never reset).
Write-Host "Clearing prior load-test state (Redis + request log)..."
& docker compose -f $composeFile exec -T redis redis-cli FLUSHALL | Out-Null
& docker compose -f $composeFile exec -T postgres psql -U switchyard -d switchyard -c "TRUNCATE requests, requests_daily" | Out-Null

$processes = @{}

Write-Host "Starting mock-primary on :9501 and mock-fallback on :9502..."
$processes.mockPrimary = (Start-Process -FilePath (Join-Path $binDir "mockprovider.exe") `
    -ArgumentList "-addr", ":9501", "-name", "mock-primary" `
    -RedirectStandardOutput (Join-Path $logDir "mock-primary.log") `
    -RedirectStandardError (Join-Path $logDir "mock-primary.err.log") `
    -PassThru -WindowStyle Hidden).Id

$processes.mockFallback = (Start-Process -FilePath (Join-Path $binDir "mockprovider.exe") `
    -ArgumentList "-addr", ":9502", "-name", "mock-fallback" `
    -RedirectStandardOutput (Join-Path $logDir "mock-fallback.log") `
    -RedirectStandardError (Join-Path $logDir "mock-fallback.err.log") `
    -PassThru -WindowStyle Hidden).Id

Start-Sleep -Milliseconds 300

$env:SWITCHYARD_PROVIDERS_CONFIG = Join-Path $repoRoot "scripts\loadtest\providers.yaml"
$env:SWITCHYARD_TEAMS_CONFIG = Join-Path $repoRoot "scripts\loadtest\teams.yaml"
$env:SWITCHYARD_QUALITY_CONFIG = Join-Path $repoRoot "scripts\loadtest\quality.yaml"
# Routing reuses configs/router.yaml unchanged — its policy (simple->fast,
# complex->frontier) already matches the tiers in loadtest/providers.yaml. Set
# to an absolute path so the gateway's working directory does not matter.
$env:SWITCHYARD_ROUTER_CONFIG = Join-Path $repoRoot "configs\router.yaml"
if ($Semantic) {
    $env:SWITCHYARD_CACHE_CONFIG = Join-Path $repoRoot "scripts\loadtest\cache-semantic.yaml"
    Write-Host "Semantic cache tier ON (real Gemini embeddings)."
} else {
    $env:SWITCHYARD_CACHE_CONFIG = Join-Path $repoRoot "scripts\loadtest\cache.yaml"
}
$env:SWITCHYARD_LOADTEST_KEY = "loadtest-dummy-key"
$env:SWITCHYARD_ADDR = ":8080"
$env:SWITCHYARD_ADMIN_ADDR = ":9090"
$env:SWITCHYARD_REDIS_ADDR = "localhost:6379"
$env:SWITCHYARD_ENV = "dev"
$env:SWITCHYARD_LOG_LEVEL = "warn"

Write-Host "Starting the gateway on :8080 (admin :9090)..."
$processes.gateway = (Start-Process -FilePath (Join-Path $binDir "switchyard-gateway.exe") `
    -RedirectStandardOutput (Join-Path $logDir "gateway.log") `
    -RedirectStandardError (Join-Path $logDir "gateway.err.log") `
    -PassThru -WindowStyle Hidden).Id

$healthy = $false
for ($i = 0; $i -lt 50; $i++) {
    Start-Sleep -Milliseconds 200
    try {
        $resp = Invoke-WebRequest -Uri "http://localhost:8080/healthz" -UseBasicParsing -TimeoutSec 2
        if ($resp.StatusCode -eq 200) { $healthy = $true; break }
    } catch {}
}
if (-not $healthy) {
    Write-Error "Gateway never became healthy. Check $logDir\gateway.err.log"
    exit 1
}

$processes | ConvertTo-Json | Set-Content -Path $pidFile -Encoding utf8

try {
    $metrics = Invoke-WebRequest -Uri "http://localhost:9090/metrics" -UseBasicParsing -TimeoutSec 2
    $m = [regex]::Match($metrics.Content, "go_goroutines\s+(\d+)")
    if ($m.Success) {
        Write-Host "Baseline goroutines: $($m.Groups[1].Value) (compare against scripts\stop-loadtest-env.ps1's reading after the run)"
    }
} catch {}

Write-Host ""
# The scripts' own handleSummary() writes the JSON summary; do not pass
# --summary-export, it races that write.
Write-Host "Environment ready. Run the load test with:"
if ($Semantic) {
    Write-Host "  k6 run scripts\loadtest-semantic.js"
} else {
    Write-Host "  k6 run scripts\loadtest.js"
}
Write-Host ""
Write-Host "When finished:"
Write-Host "  .\scripts\stop-loadtest-env.ps1"
