# Builds and starts everything scripts/loadtest.js needs: two mock providers
# (scripts/mockprovider) and the real gateway binary pointed at
# scripts/loadtest's config, all as background processes. Requires Redis
# already running (deploy/docker-compose.yml). Run
# scripts/stop-loadtest-env.ps1 when the load test is done.

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $repoRoot "bin"
$logDir = Join-Path $repoRoot "scripts\loadtest\logs"
$pidFile = Join-Path $env:TEMP "switchyard-loadtest-pids.json"

New-Item -ItemType Directory -Force -Path $binDir | Out-Null
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

$redisCheck = Test-NetConnection -ComputerName localhost -Port 6379 -WarningAction SilentlyContinue
if (-not $redisCheck.TcpTestSucceeded) {
    Write-Error "Redis is not reachable on localhost:6379. Start it first: docker compose -f deploy/docker-compose.yml up -d redis"
    exit 1
}

Write-Host "Building gateway and mock provider binaries..."
& go build -o (Join-Path $binDir "switchyard-gateway.exe") (Join-Path $repoRoot "cmd\gateway")
& go build -o (Join-Path $binDir "mockprovider.exe") (Join-Path $repoRoot "scripts\mockprovider")

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
Write-Host "Environment ready. Run the load test with:"
Write-Host "  k6 run scripts\loadtest.js"
Write-Host ""
Write-Host "When finished:"
Write-Host "  .\scripts\stop-loadtest-env.ps1"
