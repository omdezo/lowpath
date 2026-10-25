<#
.SYNOPSIS
    Installs or upgrades the Lowpath relay on a Linux server over SSH.

.DESCRIPTION
    Uploads the relay binary and relay\install.sh to a private temporary
    directory on the server, runs the installer with sudo (systemd service
    "lowpath-relay"), and prints the address and key to enter in
    Lowpath -> Settings -> Relays.

    Uses the OpenSSH client built into Windows (ssh.exe, scp.exe). With
    password logins you are asked up to three times (plus sudo); set up an SSH
    key first to avoid that.

.PARAMETER Server
    The server as you would pass it to ssh: user@host, or a Host alias from
    your ~/.ssh/config.

.PARAMETER Port
    UDP port the relay listens on (default 40000).

.PARAMETER Key
    Access key for the relay. When omitted a new random 24-character key is
    generated, replacing any key already on the server.

.PARAMETER KeepKey
    Keep the key already on the server (use when upgrading the binary).

.PARAMETER Arch
    amd64 or arm64. Detected from the server when omitted.

.PARAMETER Binary
    Relay binary to upload instead of dist\relay-linux-<arch>.

.PARAMETER Forward
    Install a forwarding hop instead of an egress: host:port of the egress
    relay this server passes packets to. A hop has no key of its own.

.PARAMETER PublicHost
    Host or IP to print as the relay address when it differs from the host
    part of -Server (for example when -Server is an ssh alias).

.EXAMPLE
    .\deploy\deploy-relay.ps1 -Server root@203.0.113.10

.EXAMPLE
    .\deploy\deploy-relay.ps1 -Server ubuntu@relay-fra.example.com -KeepKey

.EXAMPLE
    .\deploy\deploy-relay.ps1 -Server root@198.51.100.20 -Port 40001 -Forward relay-fra.example.com:40000
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Server,
    [ValidateRange(1, 65535)][int]$Port = 40000,
    [string]$Key = '',
    [switch]$KeepKey,
    [ValidateSet('amd64', 'arm64')][string]$Arch = '',
    [string]$Binary = '',
    [string]$Forward = '',
    [string]$PublicHost = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Same rule as install.sh: the key lands in a systemd EnvironmentFile.
$KeyPattern = '^[A-Za-z0-9._~+/=-]{8,128}$'

function New-RelayKey([int]$Length = 24) {
    $alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789'
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    $sb = New-Object System.Text.StringBuilder
    $buf = New-Object byte[] 64
    try {
        while ($sb.Length -lt $Length) {
            $rng.GetBytes($buf)
            foreach ($b in $buf) {
                # Rejecting bytes >= 248 (4 x 62) keeps every character equally likely.
                if ($b -lt 248 -and $sb.Length -lt $Length) {
                    [void]$sb.Append($alphabet[$b % 62])
                }
            }
        }
    }
    finally {
        $rng.Dispose()
    }
    $sb.ToString()
}

function Find-RelayBinary([string]$arch) {
    if ($Binary) {
        if (-not (Test-Path -LiteralPath $Binary -PathType Leaf)) { throw "-Binary '$Binary' not found" }
        return (Resolve-Path -LiteralPath $Binary).Path
    }
    $name = "relay-linux-$arch"
    # Repo layout: deploy\..\dist\<name>. Release layout: dist\deploy\..\<name>.
    foreach ($dir in @((Join-Path $PSScriptRoot '..\dist'), (Join-Path $PSScriptRoot '..'))) {
        $p = Join-Path $dir $name
        if (Test-Path -LiteralPath $p -PathType Leaf) { return (Resolve-Path -LiteralPath $p).Path }
    }
    throw "$name not found in dist\. Run .\build.ps1, or build it: `$env:GOOS='linux'; `$env:GOARCH='$arch'; go build -o dist\$name .\cmd\relay"
}

# --- check inputs before touching the server ---------------------------------
if ($Server -notmatch '^[\w.@:%+\[\]-]+$') { throw "-Server must look like user@host" }
if ($KeepKey -and $Key) { throw "use either -Key or -KeepKey, not both" }
if ($Key -and $Key -notmatch $KeyPattern) { throw "-Key must be 8-128 characters of A-Z a-z 0-9 . _ ~ + / = -" }
if ($Forward) {
    if ($Forward -notmatch '^[\w.:\[\]-]+:\d{1,5}$') { throw "-Forward must be host:port of the egress relay" }
    if ($Key -or $KeepKey) { throw "a -Forward hop has no key of its own; enter the egress's key in Lowpath" }
}
elseif (-not $KeepKey -and -not $Key) {
    $Key = New-RelayKey
}
foreach ($tool in 'ssh', 'scp') {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) {
        throw "$tool.exe not found. Add it in Settings > System > Optional features > OpenSSH Client."
    }
}
$installer = Join-Path $PSScriptRoot 'relay\install.sh'
if (-not (Test-Path -LiteralPath $installer)) { throw "installer not found: $installer" }
if ($Arch) { $relayBinary = Find-RelayBinary $Arch }

if (-not $PublicHost) {
    $PublicHost = $Server -replace '^.*@', ''
    if ($PublicHost -notmatch '[.:]') {
        Write-Warning "'$PublicHost' looks like an ssh alias; pass -PublicHost with the server's IP or DNS name so Lowpath can reach it"
    }
}
$address = if ($PublicHost.Contains(':')) { "[$($PublicHost.Trim('[]'))]:$Port" } else { "${PublicHost}:$Port" }

$remoteDir = '/tmp/lowpath-deploy-' + [guid]::NewGuid().ToString('N').Substring(0, 12)

# --- 1. private upload directory + architecture ------------------------------
Write-Host "==> Connecting to $Server"
$machine = & ssh $Server "mkdir -m 700 $remoteDir && uname -m"
if ($LASTEXITCODE -ne 0) { throw "ssh $Server failed (exit code $LASTEXITCODE)" }
$machine = "$($machine | Select-Object -Last 1)".Trim()

$stage = Join-Path ([System.IO.Path]::GetTempPath()) ('lowpath-deploy-' + [guid]::NewGuid().ToString('N'))
$remoteCleaned = $false # the install command removes the upload directory itself
try {
    if (-not $Arch) {
        switch -Regex ($machine) {
            '^(x86_64|amd64)$' { $Arch = 'amd64' }
            '^(aarch64|arm64)$' { $Arch = 'arm64' }
            default { throw "unsupported server architecture '$machine' (relay builds exist for amd64 and arm64)" }
        }
        $relayBinary = Find-RelayBinary $Arch
    }
    Write-Host "==> Server is linux/$Arch; uploading $(Split-Path -Leaf $relayBinary)"

    # --- 2. upload ------------------------------------------------------------
    New-Item -ItemType Directory -Path $stage | Out-Null
    Copy-Item -LiteralPath $relayBinary -Destination (Join-Path $stage 'lowpath-relay')
    # LF line endings whatever the checkout did to them; sh chokes on CRLF.
    $script = [System.IO.File]::ReadAllText($installer) -replace "`r`n", "`n"
    $utf8 = New-Object System.Text.UTF8Encoding $false
    [System.IO.File]::WriteAllText((Join-Path $stage 'install.sh'), $script, $utf8)
    $files = @('lowpath-relay', 'install.sh')
    $installArgs = "--binary $remoteDir/lowpath-relay --port $Port"
    if ($Forward) {
        $installArgs += " --forward $Forward"
    }
    elseif (-not $KeepKey) {
        # A file in the 0700 upload directory keeps the key out of process lists.
        [System.IO.File]::WriteAllText((Join-Path $stage 'key'), $Key, $utf8)
        $files += 'key'
        $installArgs += " --key-file $remoteDir/key"
    }
    Push-Location $stage # relative names: scp would read "C:\..." as a host
    try {
        & scp -q @files "${Server}:$remoteDir/"
        if ($LASTEXITCODE -ne 0) { throw "upload failed (exit code $LASTEXITCODE)" }
    }
    finally {
        Pop-Location
    }

    # --- 3. install -----------------------------------------------------------
    Write-Host "==> Installing (sudo may ask for your password)"
    # No double quotes: Windows PowerShell 5.1 mangles them in native arguments.
    $remote = 'if [ $(id -u) -eq 0 ]; then sh {0}/install.sh {1}; else sudo sh {0}/install.sh {1}; fi; rc=$?; rm -rf {0}; exit $rc' -f $remoteDir, $installArgs
    $remoteCleaned = $true
    & ssh -t $Server $remote
    if ($LASTEXITCODE -ne 0) { throw "install failed on $Server (exit code $LASTEXITCODE); see the output above" }
}
catch {
    if (-not $remoteCleaned) {
        try { & ssh $Server "rm -rf $remoteDir" | Out-Null } catch { }
    }
    throw
}
finally {
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}

# --- 4. what to paste into Lowpath -------------------------------------------
Write-Host ''
Write-Host 'Relay deployed. In Lowpath open Settings > Relays > Add relay and enter:' -ForegroundColor Green
Write-Host ''
Write-Host "    Address  $address"
if ($Forward) {
    Write-Host '    Key      the egress relay''s key'
    Write-Host "    Forwards to egress: the relay you added for $Forward"
}
elseif ($KeepKey) {
    Write-Host '    Key      (unchanged: the key already on the server, in /etc/lowpath-relay.env)'
}
else {
    Write-Host "    Key      $Key"
}
Write-Host ''
Write-Host "If your provider has a cloud firewall or security group, allow UDP $Port there too."
