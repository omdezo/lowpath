<#
.SYNOPSIS
    Builds Lowpath: the UI, the Windows, Linux and macOS apps, lpcheck and the
    Linux relays.

.DESCRIPTION
    Output in dist\:
      Lowpath.exe              the desktop app (GUI subsystem, UI embedded)
      lowpathsvc.exe           the background service (lowpathsvc install, once)
      lowpath-linux-amd64/arm64 the desktop app for Linux
      Lowpath-macos.zip        Lowpath.app for macOS 12+ (Apple silicon and Intel)
      lpcheck.exe              STUN check that shows which public IP the internet sees
      relay-linux-amd64/arm64  the relay server, for deploy\deploy-relay.ps1
      deploy\                  the deploy scripts
    Plus bin\lowpath.exe: a console build of the app for debugging (logs in
    the terminal, Ctrl+C stops it).

    Works with Windows PowerShell 5.1 and PowerShell 7.

.PARAMETER SkipTests
    Skip go vet and go test.

.PARAMETER SkipUI
    Don't rebuild the UI; embed the last build in ui\dist\app, or, when
    there is none (a fresh clone), the placeholder page that says how to
    build it.

.EXAMPLE
    .\build.ps1
    .\build.ps1 -SkipTests -SkipUI
#>
[CmdletBinding()]
param(
    [switch]$SkipTests,
    [switch]$SkipUI
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3
$root = $PSScriptRoot
$dist = Join-Path $root 'dist'
$winresVersion = 'v0.3.3'

# Native commands don't throw on failure; check their exit code.
function Invoke-Native([string]$What, [scriptblock]$Command) {
    Write-Host "==> $What" -ForegroundColor Cyan
    & $Command
    if ($LASTEXITCODE -ne 0) { throw "$What failed (exit code $LASTEXITCODE)" }
}

# Runs $Command with the given environment variables set, then restores them.
function Invoke-WithEnv([hashtable]$Vars, [scriptblock]$Command) {
    $saved = @{}
    foreach ($k in $Vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process')
        [Environment]::SetEnvironmentVariable($k, $Vars[$k], 'Process')
    }
    try { & $Command }
    finally {
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
    }
}

Push-Location $root
try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go is not installed (https://go.dev/dl/)' }

    # --- UI ---------------------------------------------------------------
    # npm run build writes ui\dist\app, which git ignores; without it the
    # programs embed the tracked ui\dist\placeholder page instead.
    $uiBuilt = Join-Path $root 'ui\dist\app\index.html'
    if ($SkipUI) {
        Write-Host '==> UI: skipped (-SkipUI), embedding the existing ui\dist\app' -ForegroundColor Yellow
    } else {
        if (-not (Get-Command npm -ErrorAction SilentlyContinue)) { throw 'npm is not installed (https://nodejs.org/); or build with -SkipUI' }
        Push-Location (Join-Path $root 'ui')
        try {
            if (-not (Test-Path 'node_modules')) { Invoke-Native 'npm ci' { npm ci } }
            Invoke-Native 'npm run build' { npm run build }
        } finally { Pop-Location }
    }
    if (-not (Test-Path $uiBuilt)) {
        Write-Warning 'No UI build in ui\dist\app: the app will show a page that says how to build it. Run without -SkipUI.'
    }

    # --- Windows resources (icon, manifest, version) ------------------------
    # The .syso is checked in; it is only regenerated when its inputs changed,
    # and a failure (e.g. offline) keeps the existing one.
    $syso = Join-Path $root 'cmd\lowpath\rsrc_windows_amd64.syso'
    $winres = Join-Path $root 'cmd\lowpath\winres'
    $newest = Get-ChildItem $winres -File | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not (Test-Path $syso) -or $newest.LastWriteTime -gt (Get-Item $syso).LastWriteTime) {
        $version = (Select-String -Path (Join-Path $root 'internal\app\server.go') -Pattern 'const Version = "([^"]+)"').Matches[0].Groups[1].Value
        Write-Host "==> go-winres (version $version)" -ForegroundColor Cyan
        & go run "github.com/tc-hib/go-winres@$winresVersion" make --in (Join-Path $winres 'winres.json') --arch amd64 `
            --out (Join-Path $root 'cmd\lowpath\rsrc') --file-version "$version.0" --product-version "$version.0"
        if ($LASTEXITCODE -ne 0) {
            if (-not (Test-Path $syso)) { throw 'go-winres failed and there is no rsrc_windows_amd64.syso' }
            Write-Warning 'go-winres failed; keeping the existing rsrc_windows_amd64.syso'
        }
    }

    # --- checks ---------------------------------------------------------------
    if ($SkipTests) {
        Write-Host '==> go vet / go test: skipped (-SkipTests)' -ForegroundColor Yellow
    } else {
        Invoke-Native 'go vet ./...' { go vet ./... }
        Invoke-Native 'go test ./...' { go test ./... }
    }

    # --- binaries -------------------------------------------------------------
    New-Item -ItemType Directory -Force $dist | Out-Null
    New-Item -ItemType Directory -Force (Join-Path $root 'bin') | Out-Null
    Invoke-WithEnv @{ GOOS = 'windows'; GOARCH = 'amd64' } {
        Invoke-Native 'dist\Lowpath.exe' { go build -trimpath -ldflags '-H windowsgui -s -w' -o (Join-Path $dist 'Lowpath.exe') ./cmd/lowpath }
        Invoke-Native 'dist\lowpathsvc.exe' { go build -trimpath -ldflags '-s -w' -o (Join-Path $dist 'lowpathsvc.exe') ./cmd/lowpathsvc }
        Invoke-Native 'bin\lowpathsvc.exe (for the end-to-end test)' { go build -o (Join-Path $root 'bin\lowpathsvc.exe') ./cmd/lowpathsvc }
        Invoke-Native 'dist\lpcheck.exe' { go build -trimpath -ldflags '-s -w' -o (Join-Path $dist 'lpcheck.exe') ./cmd/lpcheck }
        Invoke-Native 'bin\lowpath.exe (console, for debugging)' { go build -o (Join-Path $root 'bin\lowpath.exe') ./cmd/lowpath }
    }
    foreach ($arch in @('amd64', 'arm64')) {
        Invoke-WithEnv @{ GOOS = 'linux'; GOARCH = $arch; CGO_ENABLED = '0' } {
            Invoke-Native "dist\relay-linux-$arch" { go build -trimpath -ldflags '-s -w' -o (Join-Path $dist "relay-linux-$arch") ./cmd/relay }
            Invoke-Native "dist\lowpath-linux-$arch" { go build -trimpath -ldflags '-s -w' -o (Join-Path $dist "lowpath-linux-$arch") ./cmd/lowpath }
        }
    }
    # macOS: one universal Lowpath.app for Apple silicon and Intel Macs.
    $macBins = @()
    foreach ($arch in @('arm64', 'amd64')) {
        $out = Join-Path $root "bin\lowpath-darwin-$arch"
        Invoke-WithEnv @{ GOOS = 'darwin'; GOARCH = $arch; CGO_ENABLED = '0' } {
            Invoke-Native "bin\lowpath-darwin-$arch" { go build -trimpath -ldflags '-s -w' -o $out ./cmd/lowpath }
        }
        $macBins += $out
    }
    $version = (Select-String -Path (Join-Path $root 'internal\app\server.go') -Pattern 'Version\s*=\s*"([^"]+)"').Matches[0].Groups[1].Value
    # A zip keeps the program's executable mode, which a folder copied from
    # Windows loses.
    Invoke-Native 'dist\Lowpath-macos.zip (Lowpath.app)' { go run ./scripts/macapp -out (Join-Path $dist 'Lowpath-macos.zip') -version $version @macBins }

    Write-Host '==> dist\deploy' -ForegroundColor Cyan
    $deployOut = Join-Path $dist 'deploy'
    if (Test-Path $deployOut) { Remove-Item -Recurse -Force $deployOut }
    Copy-Item -Recurse (Join-Path $root 'deploy') $deployOut

    Write-Host ''
    Write-Host 'Built:' -ForegroundColor Green
    Get-ChildItem $dist -File | ForEach-Object { '  dist\{0,-22} {1,8:N0} KB' -f $_.Name, ($_.Length / 1KB) }
    '  bin\lowpath.exe (console build)'
} finally {
    Pop-Location
}
