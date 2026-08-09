#!/usr/bin/env pwsh
# Universal installer for the Ferngeist Gateway (Windows).
# Downloads the latest release zip, verifies its SHA-256 against SHA256SUMS,
# installs the daemon as a per-user scheduled task (needs one UAC prompt to
# create the task), and adds the CLI to the user PATH. Updates are manual:
# run `ferngeist-gateway update` when a new release is announced.
#
# Usage:
#   irm https://arafatamim.github.io/ferngeist-acp-gateway/install.ps1 | iex
#   powershell -ExecutionPolicy Bypass -File .\install.ps1
#   powershell -ExecutionPolicy Bypass -File .\install.ps1 -Lan -Yes
#
# Flags:
#   -Lan            expose the gateway on the local network (listen 0.0.0.0)
#                   default: localhost-only
#   -Localhost      force localhost-only (overrides -Lan)
#   -Yes            skip the confirmation prompt (non-interactive / CI)
#   -KeepDownloads  keep the downloaded zip in the install dir (default removes it)
[CmdletBinding()]
param(
    [switch]$Yes,
    [switch]$Lan,
    [switch]$Localhost,
    [switch]$KeepDownloads
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$RepoBase = 'https://arafatamim.github.io/ferngeist-acp-gateway'
$GitRepo = 'https://github.com/arafatamim/ferngeist-acp-gateway'
$ApiBase = 'https://api.github.com/repos/arafatamim/ferngeist-acp-gateway'
$AssetBase = 'https://github.com/arafatamim/ferngeist-acp-gateway/releases/download'

function Write-Step { Write-Host "==> $args" }
function Write-Warn { Write-Host "WARNING: $args" -ForegroundColor Yellow }
function Throw-Err { throw "ERROR: $args" }

# ---------------------------------------------------------------------------
# Prereq checks
# ---------------------------------------------------------------------------
if ($PSVersionTable.PSVersion.Major -lt 5) {
    Throw-Err "PowerShell 5.1 or later is required (found $($PSVersionTable.PSVersion))."
}
if ($env:OS -ne 'Windows_NT') {
    Throw-Err "This installer is for Windows only (found OS=$env:OS)."
}
if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
    Throw-Err "curl.exe not found (inbox on Windows 10 1803+). Install it and re-run."
}
if (-not (Get-Command tar.exe -ErrorAction SilentlyContinue)) {
    Throw-Err "tar.exe not found (inbox on Windows 10 1809+). Install it and re-run."
}

# ---------------------------------------------------------------------------
# Confirm
# ---------------------------------------------------------------------------
if (-not $Yes) {
    $ans = Read-Host 'Download and install the Ferngeist Gateway daemon for this user? [y/N]'
    if ($ans -notmatch '^(y|yes)$') {
        Write-Host 'Install cancelled.'
        exit 0
    }
}

# ---------------------------------------------------------------------------
# Resolve the latest release
# ---------------------------------------------------------------------------
Write-Step 'Resolving the latest release'
$release = Invoke-RestMethod -UseBasicParsing "$ApiBase/releases/latest"
$tag = $release.tag_name
if ([string]::IsNullOrWhiteSpace($tag)) {
    Throw-Err 'could not resolve the latest release tag'
}
$ver = $tag -replace '^v', ''
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { Throw-Err "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}
$asset = "ferngeist-gateway_${ver}_windows_${arch}.zip"
Write-Step "Latest release: $tag (asset $asset)"

# ---------------------------------------------------------------------------
# Download + verify
# ---------------------------------------------------------------------------
$setupDir = Join-Path $env:TEMP 'ferngeist-gateway-setup'
New-Item -ItemType Directory -Force -Path $setupDir | Out-Null

$zipPath = Join-Path $setupDir $asset
$sumsPath = Join-Path $setupDir 'SHA256SUMS'

Write-Step "Downloading $AssetBase/$tag/$asset"
& curl.exe -fL --progress-bar "$AssetBase/$tag/$asset" -o $zipPath
if ($LASTEXITCODE -ne 0) { Throw-Err "download failed for $asset (curl exit $LASTEXITCODE)" }

& curl.exe -fsSL "$AssetBase/$tag/SHA256SUMS" -o $sumsPath
if ($LASTEXITCODE -ne 0) { Throw-Err "download failed for SHA256SUMS (curl exit $LASTEXITCODE)" }

Write-Step 'Verifying sha256 checksum'
$sumLine = Get-Content $sumsPath | Where-Object {
    ($_ -split '\s+') -contains $asset
} | Select-Object -First 1
if (-not $sumLine) { Throw-Err "no checksum for $asset in SHA256SUMS" }
$expected = ($sumLine -split '\s+')[0].Trim().ToLowerInvariant()
# Get-FileHash is absent on some PS 5.1 builds; use .NET directly (works everywhere).
$stream = [System.IO.File]::OpenRead($zipPath)
try {
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $actual = ([System.BitConverter]::ToString($sha.ComputeHash($stream)) -replace '-', '').ToLowerInvariant()
} finally {
    $stream.Dispose()
}
if ($expected -ne $actual) {
    Throw-Err "checksum mismatch for $asset (expected $expected, got $actual)"
}

# ---------------------------------------------------------------------------
# Extract
# ---------------------------------------------------------------------------
$extractDir = Join-Path $setupDir 'extract'
New-Item -ItemType Directory -Force -Path $extractDir | Out-Null
& tar.exe -xf $zipPath -C $extractDir
if ($LASTEXITCODE -ne 0) { Throw-Err "extract failed for $asset (tar exit $LASTEXITCODE)" }

$exe = Get-ChildItem -Path $extractDir -Recurse -Filter 'ferngeist-gateway.exe' | Select-Object -First 1
if (-not $exe) { Throw-Err "ferngeist-gateway.exe not found in $asset" }

# ---------------------------------------------------------------------------
# Install + start the daemon
# ---------------------------------------------------------------------------
$daemonArgs = @('daemon', 'install')
if ($Lan -and -not $Localhost) { $daemonArgs += '--lan' }
if ($Lan -and -not $Localhost) {
    Write-Step 'Installing + starting the daemon (per-user scheduled task, LAN enabled)'
} else {
    Write-Step 'Installing + starting the daemon (per-user scheduled task, localhost only)'
}
# Creating the scheduled task requires elevation (Task Scheduler denies task
# creation from a UAC-filtered medium-integrity token). Start-Process -Verb
# RunAs triggers the UAC prompt and runs the binary elevated. It returns
# immediately, so $LASTEXITCODE is not populated; success/failure is inferred
# by re-checking the task afterwards.
$daemonInstall = $exe.FullName
try {
    $proc = Start-Process -FilePath $daemonInstall -ArgumentList $daemonArgs -Verb RunAs -PassThru -Wait
    if ($proc.ExitCode -ne 0) {
        Write-Warn "daemon install exited with code $($proc.ExitCode); the binary is at $daemonInstall - run 'ferngeist-gateway daemon install' from an elevated prompt"
    }
} catch {
    # User cancelled the UAC prompt or elevation failed.
    Write-Warn "daemon install needs elevation and could not run: $($_.Exception.Message)"
}

# ---------------------------------------------------------------------------
# Add the CLI to the user PATH
# ---------------------------------------------------------------------------
$binDir = Join-Path $env:LOCALAPPDATA 'FerngeistGateway\service\bin'
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath -notlike "*$binDir*") {
    [Environment]::SetEnvironmentVariable('Path', "$binDir;$userPath", 'User')
    Write-Step "Added $binDir to your user PATH (new terminals will pick it up)."
}

# ---------------------------------------------------------------------------
# Cleanup + done
# ---------------------------------------------------------------------------
if (-not $KeepDownloads) {
    Remove-Item -Recurse -Force -Path $setupDir -ErrorAction SilentlyContinue
}
# The elevated process ran invisibly; confirm the scheduled task exists.
$task = schtasks /Query /TN FerngeistGateway /FO LIST 2>&1
if ($LASTEXITCODE -eq 0 -and $task -match 'FerngeistGateway') {
    Write-Step "Installed ferngeist-gateway $ver. The daemon is registered as the FerngeistGateway scheduled task and running."
} else {
    Write-Warn "Could not confirm the FerngeistGateway scheduled task. Run 'ferngeist-gateway daemon install' from an elevated prompt."
}
Write-Host 'Check the daemon with:  ferngeist-gateway daemon status'
Write-Host 'Updates are manual: run `ferngeist-gateway update` when a new release is announced.'
