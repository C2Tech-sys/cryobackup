# build.ps1 - build the CryoBackup agent MSI (deploy/windows/README.md).
#
#   $env:CRYO_RESTIC_EXE = 'C:\path\to\restic_0.19.1_windows_amd64.exe'
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\build.ps1
#
# or "make msi". Steps: discovery -> verify restic -> compile the Event Log
# message table (eventlog\build-messages.ps1) -> build cryo-agent.exe (the
# restic pin compiled in) and the custom-action DLL -> sign them (only when a
# signing command is configured) -> build the MSI with WiX 5.0.2 -> sign it ->
# verify (deploy\windows\verify.ps1).
#
# Exit codes: 0 built (and verified), 2 a tool or input is missing, 3 restic.exe
# is not the pinned file, 4 a build step failed, 5 signing failed, 6 the built
# MSI did not verify.

# ---- configuration (environment, with defaults) ------------------------------
$ResticExe    = $env:CRYO_RESTIC_EXE          # required: restic.exe as unzipped from the pinned upstream release
$MsiVersion   = $env:CRYO_MSI_VERSION         # optional: X.Y.Z or X.Y.Z.N; default derived from git (see below)
$Manufacturer = $env:CRYO_MSI_MANUFACTURER    # optional: ARP publisher; default "CryoBackup"
$OutDir       = $env:CRYO_MSI_OUT             # optional: output folder; default <repo>\dist\windows
$SignCommand  = $env:CRYO_SIGN_COMMAND        # optional: signing command, called once per file with the file path (sign.ps1)
$WixCommand   = $env:CRYO_WIX                 # optional: a wix.exe / wix.dll 5.0.2 to use instead of "dotnet tool run wix"
$CCompiler    = $env:CC                       # optional: C compiler for the custom-action DLL; default gcc (MinGW-w64)

# Native tools report through exit codes, checked after every call; cmdlets that
# can fail are called with -ErrorAction Stop inside try.
$ErrorActionPreference = 'Continue'
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$PinFile  = Join-Path $PSScriptRoot 'restic.pin'
$Module   = 'github.com/c2tech-sys/cryobackup'
$WixVersion = '5.0.2'
# MinGW-w64 GCC major versions cryoca.dll is built and tested with (README:
# "Build"): 16 on the release machine (WinLibs 16.2.0 UCRT), 15 on the CI runner
# (MinGW-Builds 15.2.0), where the MSI is installed for real. Another major is
# refused rather than shipped untested.
$GccMajors = @('15', '16')
if (-not $Manufacturer) { $Manufacturer = 'CryoBackup' }
if (-not $OutDir)       { $OutDir = Join-Path $RepoRoot 'dist\windows' }
if (-not $CCompiler)    { $CCompiler = 'gcc' }

function Say($m)  { Write-Host "build: $m" }
function Die($code, $m) { Write-Host "build: FAILED: $m"; exit $code }

. (Join-Path $PSScriptRoot 'pin.ps1')

# ---- discovery ---------------------------------------------------------------
$pin = Read-ResticPin $PinFile
if (-not $pin) { Die 2 "the restic pin file $PinFile is missing or incomplete" }

foreach ($tool in @('go', 'git', $CCompiler)) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { Die 2 "$tool is not on PATH" }
}
$ccMachine = "$(& $CCompiler -dumpmachine 2>$null)"
$ccVersion = "$(& $CCompiler -dumpfullversion 2>$null)"
$ccBanner  = "$(& $CCompiler --version 2>$null | Select-Object -First 1)"
Say "C compiler for cryoca.dll: $ccBanner (target $ccMachine, version $ccVersion)"
if ($ccMachine -notlike 'x86_64-w64-mingw32*') { Die 2 "the C compiler must be a MinGW-w64 gcc targeting x86_64-w64-mingw32 (it targets '$ccMachine')" }
if ($GccMajors -notcontains $ccVersion.Split('.')[0]) {
    Die 2 "MinGW-w64 gcc major version $($GccMajors -join ' or ') is required for cryoca.dll (README), found '$ccVersion'"
}
if (-not $WixCommand -and -not (Get-Command dotnet -ErrorAction SilentlyContinue)) {
    Die 2 'dotnet (the .NET SDK) is not on PATH; it restores WiX from .config\dotnet-tools.json (or set CRYO_WIX)'
}
if (-not $ResticExe) { Die 2 "CRYO_RESTIC_EXE is not set: point it at restic $($pin.version) windows amd64 (deploy\windows\fetch-restic.ps1 downloads and checks it)" }
if (-not (Test-Path -LiteralPath $ResticExe -PathType Leaf)) { Die 2 'CRYO_RESTIC_EXE does not name a file' }

# Version. A tag vX.Y.Z on HEAD gives X.Y.Z.0; N commits after it give X.Y.Z.N;
# no tag gives 0.0.0.N (N = commits in HEAD). Windows Installer compares only
# X.Y.Z, so builds that differ in N replace each other in place
# (AllowSameVersionUpgrades); a real release bumps X.Y.Z with a tag. The RMM
# wrapper (rmm\install-cryobackup.ps1) compares all four fields: it applies a
# higher N and refuses a lower one.
$describe = (& git -C $RepoRoot describe --tags --always --dirty 2>$null)
$commit   = (& git -C $RepoRoot rev-parse --short HEAD 2>$null)
if (-not $describe) { $describe = 'dev' }
if (-not $commit)   { $commit = 'unknown' }
if (-not $MsiVersion) {
    $tag = (& git -C $RepoRoot describe --tags --abbrev=0 --match 'v[0-9]*' 2>$null)
    if ($LASTEXITCODE -eq 0 -and $tag -match '^v(\d+)\.(\d+)\.(\d+)$') {
        $n = (& git -C $RepoRoot rev-list --count "$tag..HEAD")
        $MsiVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3]).$n"
    } else {
        $n = (& git -C $RepoRoot rev-list --count HEAD)
        $MsiVersion = "0.0.0.$n"
    }
}
if ($MsiVersion -notmatch '^(\d+)\.(\d+)\.(\d+)(\.(\d+))?$' -or [int]$Matches[1] -gt 255 -or [int]$Matches[2] -gt 255 -or [int]$Matches[3] -gt 65535) {
    Die 2 "MSI version '$MsiVersion' is not X.Y.Z[.N] with X,Y <= 255 and Z <= 65535"
}

# The version the agent reports (cryo-agent status, "version", the enrollment and
# every heartbeat). A release is built with an explicit CRYO_MSI_VERSION: the
# agent then reports exactly that version, so the binary, the console and the MSI
# agree (M2 lab finding F6: the 0.2.0 MSI's agent reported its git describe,
# v0.1.0-83-g...). A build without one is a dev build and keeps the git describe
# string, which says which commit it is.
$AgentVersion = $describe
if ($env:CRYO_MSI_VERSION) { $AgentVersion = $MsiVersion }
Say "version $MsiVersion (agent reports $AgentVersion, git $describe, commit $commit)"

# ---- verify restic -----------------------------------------------------------
$got = (Get-FileHash -LiteralPath $ResticExe -Algorithm SHA256).Hash.ToLowerInvariant()
if ($got -ne $pin.exe_sha256) {
    Die 3 "restic.exe SHA-256 is $got, the pin is $($pin.exe_sha256) (restic $($pin.version)); refusing to package it"
}
Say "restic.exe matches the pin (restic $($pin.version), SHA-256 $got)"

# ---- build -------------------------------------------------------------------
$Stage = Join-Path $OutDir 'stage'
try {
    if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force -ErrorAction Stop }
    New-Item -ItemType Directory -Force -Path $Stage -ErrorAction Stop | Out-Null
} catch { Die 4 "the staging folder could not be prepared: $($_.Exception.Message)" }

# The Event Log message table, compiled from eventlog\messages.mc into the .syso
# go build links into cryo-agent.exe (the source's EventMessageFile). The
# committed .syso is regenerated here, so the MSI never ships a stale table.
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'eventlog\build-messages.ps1')
if ($LASTEXITCODE -ne 0) { Die 4 'the Event Log message table did not compile (eventlog\build-messages.ps1)' }

$date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
$ldflags = "-s -w -X $Module/internal/buildinfo.Version=$AgentVersion -X $Module/internal/buildinfo.Commit=$commit -X $Module/internal/buildinfo.Date=$date -X $Module/internal/agent.PinnedResticSHA256=$($pin.exe_sha256)"
$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
Push-Location $RepoRoot
try {
    & go build -trimpath -ldflags $ldflags -o (Join-Path $Stage 'cryo-agent.exe') ./cmd/cryo-agent
    $rc = $LASTEXITCODE
} finally {
    Pop-Location
    $env:GOOS = $saved.GOOS; $env:GOARCH = $saved.GOARCH; $env:CGO_ENABLED = $saved.CGO_ENABLED
}
if ($rc -ne 0) { Die 4 'go build of cryo-agent.exe failed' }
Say 'cryo-agent.exe built with the restic pin'

& $CCompiler -O2 -Wall -Wextra -Werror -shared -s -static-libgcc -o (Join-Path $Stage 'cryoca.dll') (Join-Path $PSScriptRoot 'ca\cryoca.c') -lmsi -lbcrypt
if ($LASTEXITCODE -ne 0) { Die 4 'the custom-action DLL did not compile' }
Say 'cryoca.dll built'

try { Copy-Item -LiteralPath $ResticExe -Destination (Join-Path $Stage 'restic.exe') -ErrorAction Stop }
catch { Die 4 "restic.exe could not be staged: $($_.Exception.Message)" }
$staged = (Get-FileHash -LiteralPath (Join-Path $Stage 'restic.exe') -Algorithm SHA256).Hash.ToLowerInvariant()
if ($staged -ne $pin.exe_sha256) { Die 3 'the staged restic.exe changed while it was copied' }

# ---- sign what goes into the MSI (never restic.exe) --------------------------
. (Join-Path $PSScriptRoot 'sign.ps1')
$Signing = [bool]$SignCommand
if ($Signing) {
    foreach ($f in @('cryo-agent.exe', 'cryoca.dll')) {
        if (-not (Invoke-CryoSign $SignCommand (Join-Path $Stage $f))) { Die 5 "signing $f failed" }
    }
} else {
    Write-Host ''
    Write-Host '*** UNSIGNED BUILD: CRYO_SIGN_COMMAND is not set. cryo-agent.exe, cryoca.dll and the MSI ***'
    Write-Host '*** are NOT Authenticode-signed. Do not ship this build (ADR 0006: sign locally).    ***'
    Write-Host ''
}

# ---- MSI ---------------------------------------------------------------------
if ($WixCommand) {
    if ($WixCommand -like '*.dll') { $wix = @('dotnet', $WixCommand) } else { $wix = @($WixCommand) }
} else {
    Push-Location $RepoRoot
    try { & dotnet tool restore | Out-Host; $rc = $LASTEXITCODE } finally { Pop-Location }
    if ($rc -ne 0) { Die 2 'dotnet tool restore failed (WiX 5.0.2 from .config\dotnet-tools.json)' }
    # Run the restored wix.dll directly: "dotnet tool run wix" drops every repeated
    # "-d" after the first, so Manufacturer and StageDir never reached WiX.
    $nuget = $env:NUGET_PACKAGES; if (-not $nuget) { $nuget = Join-Path $env:USERPROFILE '.nuget\packages' }
    $wixDll = Join-Path $nuget "wix\$WixVersion\tools\net6.0\any\wix.dll"
    if (-not (Test-Path -LiteralPath $wixDll)) { Die 2 "WiX $WixVersion not found after restore: $wixDll" }
    $wix = @('dotnet', $wixDll)
}
$wixExe = $wix[0]; $wixPre = @(); if ($wix.Count -gt 1) { $wixPre = $wix[1..($wix.Count - 1)] }

Push-Location $RepoRoot
try { $wv = & $wixExe @wixPre --version 2>$null } finally { Pop-Location }
if (-not ("$wv" -like "$WixVersion*")) { Die 2 "WiX $WixVersion is required (ADR 0007), found '$wv'" }
Say "WiX $wv"

$Msi = Join-Path $OutDir 'cryobackup-agent.msi'
if (Test-Path -LiteralPath $Msi) { Remove-Item -LiteralPath $Msi -Force -ErrorAction SilentlyContinue }
Push-Location $RepoRoot
try {
    & $wixExe @wixPre build (Join-Path $PSScriptRoot 'CryoBackup.wxs') -arch x64 -pdbtype none `
        -d "Version=$MsiVersion" -d "Manufacturer=$Manufacturer" -d "StageDir=$Stage" -o $Msi
    $rc = $LASTEXITCODE
} finally { Pop-Location }
if ($rc -ne 0 -or -not (Test-Path -LiteralPath $Msi)) { Die 4 'wix build failed' }
Say "MSI built: $Msi"

if ($Signing) {
    if (-not (Invoke-CryoSign $SignCommand $Msi)) { Die 5 'signing the MSI failed' }
}

# ---- verify ------------------------------------------------------------------
$env:CRYO_VERIFY_MSI = $Msi
$env:CRYO_VERIFY_SIGNED = $(if ($Signing) { '1' } else { '0' })
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'verify.ps1')
if ($LASTEXITCODE -ne 0) { Die 6 'the built MSI did not verify' }

# ---- report ------------------------------------------------------------------
Say "done: $Msi (version $MsiVersion, $(if ($Signing) { 'signed' } else { 'UNSIGNED' }))"
exit 0
