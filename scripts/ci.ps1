<#
.SYNOPSIS
    Lowpath's CI pipeline, run locally (no GitHub Actions minutes needed).

.DESCRIPTION
    Stages, in order (stops at the first failure):
      format    gofmt finds nothing to change
      vet       go vet ./...
      test      go test ./...
      cross     app + relay + lpcheck build for linux/amd64, linux/arm64,
                darwin/arm64, darwin/amd64 and windows/arm64; the whole module
                (tests included) vets for linux and darwin
      ui        npm ci (when needed) + typecheck + production build
      build     dist\Lowpath.exe, dist\lowpathsvc.exe and dist\lpcheck.exe
      e2e       end-to-end UI test (only with -E2E)

    Writes a summary table and a log per stage to .ci\.

.PARAMETER Quick
    Skip the slow engine tests (go test -short) and the UI build.

.PARAMETER E2E
    Also run the end-to-end UI test (needs Microsoft Edge).

.PARAMETER Report
    Publish the result as the GitHub commit status "local-ci" on HEAD, so the
    pull request shows a green or red check without GitHub Actions. Needs the
    GitHub CLI (on PATH, or its path in $env:LOWPATH_GH) signed in with repo
    scope, a pushed HEAD, and a clean working tree with no untracked files
    (the status must describe exactly the pushed commit). Reporting is best
    effort: if it can't publish, it says why and the CI result is unchanged.

.EXAMPLE
    .\scripts\ci.ps1
    .\scripts\ci.ps1 -Quick
    .\scripts\ci.ps1 -E2E
    .\scripts\ci.ps1 -E2E -Report     # before asking for review on a PR
#>
[CmdletBinding()]
param(
    [switch]$Quick,
    [switch]$E2E,
    [switch]$Report
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
    if (-not $ok) {
        Show-Summary
        Write-Host "Stage '$Name' failed; log: $log" -ForegroundColor Red
        Publish-Status 'failure' "$Name failed ($(Get-StageList))"
        exit 1
    }
}

function Get-StageList {
    ($results | ForEach-Object { "$($_.Stage) $($_.Result.ToLower())" }) -join ', '
}

# Get-ReportTarget returns the GitHub repo and commit a -Report run describes,
# or $null (after saying why) when the result can't honestly describe exactly
# one commit: uncommitted or untracked files would be built but not pushed.
# ui/dist is build output (the ui stage rewrites it), not source, so it
# doesn't count.
function Get-ReportTarget {
    try {
        if (git -C $root status --porcelain -- . ':(exclude)ui/dist') {
            Write-Host 'Not reporting: the working tree has uncommitted or untracked changes.' -ForegroundColor Yellow
            return $null
        }
        $sha = "$(git -C $root rev-parse HEAD)".Trim()
        $remote = "$(git -C $root remote get-url origin)".Trim()
        if (-not $sha) {
            Write-Host 'Not reporting: no commit to report on.' -ForegroundColor Yellow
            return $null
        }
        if ($remote -notmatch 'github\.com[:/](?<repo>[^/]+/[^/]+?)(\.git)?/?$') {
            Write-Host "Not reporting: origin ($remote) is not a GitHub repository." -ForegroundColor Yellow
            return $null
        }
        return [pscustomobject]@{ Repo = $Matches['repo']; Sha = $sha }
    } catch {
        Write-Host "Not reporting: $_" -ForegroundColor Yellow
        return $null
    }
}

# Publish-Status records the run as the "local-ci" commit status on GitHub.
# Reporting is best effort: a problem here never changes the CI result.
function Publish-Status([string]$State, [string]$Description) {
    if (-not $Report) { return }
    try {
        if (-not $reportTarget) {
            Write-Host 'Not reporting: see the warning at the start of the run.' -ForegroundColor Yellow
            return
        }
        $target = Get-ReportTarget
        if (-not $target) { return }
        if ($target.Sha -ne $reportTarget.Sha) {
            Write-Host 'Not reporting: HEAD moved while CI was running.' -ForegroundColor Yellow
            return
        }
        $gh = if ($env:LOWPATH_GH) { $env:LOWPATH_GH } else { 'gh' }
        if (-not (Get-Command $gh -ErrorAction SilentlyContinue)) {
            Write-Host "Not reporting: GitHub CLI not found ($gh). Install it or set `$env:LOWPATH_GH." -ForegroundColor Yellow
            return
        }
        $mode = if ($Quick) { 'quick' } else { 'full' }
        if ($E2E) { $mode += ' + e2e' }
        $desc = "[$mode] $Description"
        if ($desc.Length -gt 140) { $desc = $desc.Substring(0, 137) + '...' }
        & $gh api "repos/$($target.Repo)/statuses/$($target.Sha)" -f state=$State -f context=local-ci -f "description=$desc" | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Write-Host "Reported local-ci = $State on $($target.Sha.Substring(0, 7))." -ForegroundColor Green
        } else {
            Write-Host 'Could not report the status to GitHub (is gh signed in, and is the commit pushed?).' -ForegroundColor Yellow
        }
    } catch {
        Write-Host "Could not report the status to GitHub: $_" -ForegroundColor Yellow
    }
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

# With -Report, check up front that the result can be published, so a dirty
# tree is caught before the run instead of after it.
$reportTarget = if ($Report) { Get-ReportTarget } else { $null }

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
            @{ GOOS = 'darwin'; GOARCH = 'arm64' }, @{ GOOS = 'darwin'; GOARCH = 'amd64' },
            @{ GOOS = 'windows'; GOARCH = 'arm64' }
        )
        foreach ($t in $targets) {
            $out = Join-Path $root "dist\cross\$($t.GOOS)-$($t.GOARCH)\"
            New-Item -ItemType Directory -Force $out | Out-Null
            Invoke-WithEnv @{ GOOS = $t.GOOS; GOARCH = $t.GOARCH; CGO_ENABLED = '0' } {
                go build -trimpath -o $out ./cmd/lowpath ./cmd/lowpathsvc ./cmd/relay ./cmd/lpcheck
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
        go build -trimpath -ldflags '-s -w' -o dist\lowpathsvc.exe ./cmd/lowpathsvc
        if ($LASTEXITCODE -ne 0) { throw 'service build failed' }
        go build -trimpath -o bin\lowpathsvc.exe ./cmd/lowpathsvc
        if ($LASTEXITCODE -ne 0) { throw 'service console build failed' }
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
    $total = [math]::Round(($results | Measure-Object Seconds -Sum).Sum)
    Publish-Status 'success' "all stages passed in ${total}s ($(Get-StageList))"
} finally {
    Pop-Location
}
