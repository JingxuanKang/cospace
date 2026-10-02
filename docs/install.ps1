# CoSpace guest tool installer for Windows PowerShell 5.1+.
#   irm https://raw.githubusercontent.com/JingxuanKang/cospace/master/docs/install.ps1 | iex
# Safe to re-run: an existing install is left alone when it is already the
# published version and updated in place otherwise.
$ErrorActionPreference = "Stop"
# Windows PowerShell 5.1 may default to TLS 1.0, which GitHub rejects.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

# A running exe cannot be overwritten but can be renamed; move it aside first.
function Install-Exe([string]$Source, [string]$Target) {
    if (Test-Path -LiteralPath $Target) {
        $Old = "$Target.old"
        Remove-Item -LiteralPath $Old -Force -ErrorAction SilentlyContinue
        Rename-Item -LiteralPath $Target -NewName (Split-Path -Leaf $Old) -Force
    }
    Copy-Item -LiteralPath $Source -Destination $Target -Force
}

# Release assets on GitHub; VERSION is published with every release.
$Base = "https://github.com/JingxuanKang/cospace/releases/latest/download"
$MachineArch = if ($env:PROCESSOR_ARCHITEW6432) {
    $env:PROCESSOR_ARCHITEW6432
} else {
    $env:PROCESSOR_ARCHITECTURE
}
$GoArch = switch ($MachineArch.ToUpperInvariant()) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "cospace: unsupported Windows architecture '$MachineArch'" }
}

if (-not (Get-Command ssh.exe -ErrorAction SilentlyContinue) -or
    -not (Get-Command ssh-keygen.exe -ErrorAction SilentlyContinue)) {
    throw "cospace: Windows OpenSSH Client is required. Install the 'OpenSSH Client' optional feature, then run this command again."
}

function Write-NextSteps {
    Write-Host "Next: run the 'cospace pair ...' line your host sent you, then ssh <space-name>"
}

# The published version; empty if the download host does not serve it.
$Latest = ""
try {
    $Latest = (Invoke-RestMethod -UseBasicParsing -Uri "$Base/VERSION").ToString().Trim()
} catch {}

$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\CoSpace\bin"
$InstallPath = Join-Path $InstallDir "cospace.exe"

$Existing = Get-Command cospace.exe -ErrorAction SilentlyContinue
if ($Existing) {
    $Current = ""
    try {
        $Current = ((& $Existing.Source version 2>$null) -split "\s+")[1]
    } catch {}
    if ($Latest -and $Current -eq $Latest) {
        Write-Host "cospace: already up to date ($Current) at $($Existing.Source)"
        Write-NextSteps
        return
    }
    # Replace the copy that is on PATH so the update takes effect immediately.
    $InstallDir = Split-Path -Parent $Existing.Source
    $InstallPath = $Existing.Source
    $shown = if ($Current) { $Current } else { "unknown version" }
    $target = if ($Latest) { $Latest } else { "latest" }
    Write-Host "cospace: updating $shown -> $target"
}
$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("cospace-" + [guid]::NewGuid().ToString("N"))
$Archive = Join-Path $TempDir "cospace.zip"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null
try {
    Write-Host "cospace: downloading cospace (windows/$GoArch)..."
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/cospace_windows_$GoArch.zip" -OutFile $Archive
    Expand-Archive -Path $Archive -DestinationPath $TempDir -Force
    $Downloaded = Join-Path $TempDir "cospace.exe"
    if (-not (Test-Path -LiteralPath $Downloaded)) {
        throw "cospace: downloaded archive does not contain cospace.exe"
    }
    Install-Exe $Downloaded $InstallPath
    Install-Exe $Downloaded (Join-Path $InstallDir "co.exe")
} finally {
    if (Test-Path -LiteralPath $TempDir) {
        Remove-Item -LiteralPath $TempDir -Recurse -Force
    }
}

$UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$InstallDirKey = $InstallDir.TrimEnd('\')
$AlreadyOnPath = @($UserPath -split ';') | Where-Object {
    [string]::Equals($_.Trim().TrimEnd('\'), $InstallDirKey, [StringComparison]::OrdinalIgnoreCase)
}
if (-not $AlreadyOnPath) {
    $NewUserPath = if ([string]::IsNullOrWhiteSpace($UserPath)) {
        $InstallDir
    } else {
        $UserPath.TrimEnd(';') + ";" + $InstallDir
    }
    [Environment]::SetEnvironmentVariable("Path", $NewUserPath, "User")
}
if (-not ((@($env:Path -split ';')) | Where-Object {
    [string]::Equals($_.Trim().TrimEnd('\'), $InstallDirKey, [StringComparison]::OrdinalIgnoreCase)
})) {
    $env:Path = $InstallDir + ";" + $env:Path
}

$Installed = ""
try { $Installed = ((& $InstallPath version 2>$null) -split "\s+")[1] } catch {}
Write-Host "cospace: installed cospace $Installed to $InstallPath"
Write-NextSteps
