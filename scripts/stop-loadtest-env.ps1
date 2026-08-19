# Stops the gateway and mock providers started by
# scripts/start-loadtest-env.ps1. Prints the post-run goroutine count first,
# to compare against the baseline that script printed at startup.

$ErrorActionPreference = "SilentlyContinue"

$pidFile = Join-Path $env:TEMP "switchyard-loadtest-pids.json"

try {
    $metrics = Invoke-WebRequest -Uri "http://localhost:9090/metrics" -UseBasicParsing -TimeoutSec 2
    $m = [regex]::Match($metrics.Content, "go_goroutines\s+(\d+)")
    if ($m.Success) {
        Write-Host "Goroutines before stopping: $($m.Groups[1].Value)"
    }
} catch {}

if (-not (Test-Path $pidFile)) {
    Write-Host "No $pidFile found; nothing to stop."
    exit 0
}

$processes = Get-Content $pidFile | ConvertFrom-Json
foreach ($prop in $processes.PSObject.Properties) {
    Stop-Process -Id $prop.Value -Force -Confirm:$false
    Write-Host "Stopped $($prop.Name) (PID $($prop.Value))"
}

Remove-Item $pidFile
Write-Host "Load test environment stopped."
