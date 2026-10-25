<#
.SYNOPSIS
    Lowpath's CI pipeline, run locally (no GitHub Actions minutes needed).

.DESCRIPTION
    Stages, in order (stops at the first failure):
      format    gofmt finds nothing to change
      vet       go vet ./...
      test      go test ./...
      cross     relay + lpcheck build for linux/amd64, linux/arm64, darwin/arm64,
                darwin/amd64; the whole module vets for linux and darwin
      ui        npm ci (when needed) + typecheck + production build
      build     dist\Lowpath.exe and dist\lpcheck.exe
      e2e       end-to-end UI test (only with -E2E)

    Writes a summary table and a log per stage to .ci\.

.PARAMETER Quick
    Skip the slow engine tests (go test -short) and the UI build.

.PARAMETER E2E
    Also run the end-to-end UI test (needs Microsoft Edge).

.EXAMPLE
    .\scripts\ci.ps1
    .\scripts\ci.ps1 -Quick
    .\scripts\ci.ps1 -E2E
#>
[CmdletBinding()]
param(
    [switch]$Quick,
    [switch]$E2E
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3
$root = Split-Path $PSScriptRoot -Parent
$logs = Join-Path $root '.ci'
New-Item -ItemType Directory -Force $logs | Out-Null
$results = [System.Collections.Generic.List[object]]::new()

function Invoke-Stage([string]$Name, [scriptblock]$Body) {
    Write-Host "==> $Name" -ForegroundColor Cyan
    $log = Join-Path $logs "$Name.log"
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $ok = $true
    try {
        & $Body *>&1 | Tee-Object -FilePath $log | Out-Host
        if ($LASTEXITCODE -ne 0) { $ok = $false }
    } catch {
        $_ | Out-File -Append $log
        Write-Host $_ -ForegroundColor Red
        $ok = $false
    }
    $sw.Stop()
    $results.Add([pscustomobject]@{ Stage = $Name; Result = if ($ok) { 'PASS' } else { 'FAIL' }; Seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1) })
    if (-not $ok) { Show-Summary; Write-Host "Stage '$Name' failed; log: $log" -ForegroundColor Red; exit 1 }
}

function Show-Summary {
    Write-Host ''
    $results | Format-Table -AutoSize | Out-Host
}

# Runs a native command with environment variables set just for it.
function Invoke-WithEnv([hashtable]$Vars, [scriptblock]$Command) {
    $saved = @{}
    foreach ($k in $Vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process')
        [Environment]::SetEnvironmentVariable($k, $Vars[$k], 'Process')
    }
    try { & $Command } finally {
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
    }
}

Push-Location $root
try {
    Invoke-Stage 'format' {
        $files = gofmt -l cmd internal
        if ($files) { Write-Host "Not gofmt'd:`n$files" -ForegroundColor Red; $global:LASTEXITCODE = 1 } else { $global:LASTEXITCODE = 0 }
    }

    Invoke-Stage 'vet' { go vet ./... }

    Invoke-Stage 'test' {
        if ($Quick) { go test -short ./... } else { go test ./... }
    }

    Invoke-Stage 'cross' {
        $targets = @(
            @{ GOOS = 'linux'; GOARCH = 'amd64' }, @{ GOOS = 'linux'; GOARCH = 'arm64' },
            @{ GOOS = 'darwin'; GOARCH = 'arm64' }, @{ GOOS = 'darwin'; GOARCH = 'amd64' }
        )
        foreach ($t in $targets) {
            $out = Join-Path $root "dist\cross\$($t.GOOS)-$($t.GOARCH)\"
            New-Item -ItemType Directory -Force $out | Out-Null
            Invoke-WithEnv @{ GOOS = $t.GOOS; GOARCH = $t.GOARCH; CGO_ENABLED = '0' } {
                go build -trimpath -o $out ./cmd/relay ./cmd/lpcheck
                if ($LASTEXITCODE -ne 0) { throw "build failed for $($t.GOOS)/$($t.GOARCH)" }
            }
        }
        foreach ($os in 'linux', 'darwin') {
            Invoke-WithEnv @{ GOOS = $os; GOARCH = 'amd64'; CGO_ENABLED = '0' } {
                go vet ./...
                if ($LASTEXITCODE -ne 0) { throw "vet failed for $os" }
            }
        }
        $global:LASTEXITCODE = 0
    }

    if (-not $Quick) {
        Invoke-Stage 'ui' {
            Push-Location (Join-Path $root 'ui')
            try {
                if (-not (Test-Path 'node_modules')) { npm ci; if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' } }
                npm run build
            } finally { Pop-Location }
        }
    }

    Invoke-Stage 'build' {
        go build -trimpath -ldflags '-H windowsgui -s -w' -o dist\Lowpath.exe ./cmd/lowpath
        if ($LASTEXITCODE -ne 0) { throw 'app build failed' }
        go build -trimpath -o bin\lowpath.exe ./cmd/lowpath
        if ($LASTEXITCODE -ne 0) { throw 'console build failed' }
        go build -trimpath -o dist\lpcheck.exe ./cmd/lpcheck
    }

    if ($E2E) {
        Invoke-Stage 'e2e' {
            Push-Location (Join-Path $root 'e2e')
            try {
                if (-not (Test-Path 'node_modules')) { npm ci; if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' } }
                node run.mjs
            } finally { Pop-Location }
        }
    }

    Show-Summary
    Write-Host 'CI passed.' -ForegroundColor Green
} finally {
    Pop-Location
}
