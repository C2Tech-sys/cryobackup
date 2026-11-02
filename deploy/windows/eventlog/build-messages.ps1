# build-messages.ps1 - compile the agent's Event Log message table.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\eventlog\build-messages.ps1
#
# Compiles messages.mc (beside this script) with MinGW-w64 windmc and windres
# into cmd\cryo-agent\messages_windows_amd64.syso, a COFF object go build links
# into cryo-agent.exe (windows/amd64 only; other targets never see it). The
# Event Log source "CryoBackup" names cryo-agent.exe as its EventMessageFile,
# so Event Viewer finds the text of every agent event id in the agent itself
# (EventCreate.exe, the earlier message file, only has ids 1 to 1000).
#
# build.ps1 runs this before it builds cryo-agent.exe. The .syso is committed
# so that a plain "go build" or "go test" has the table too; rerun this script
# and commit the result whenever messages.mc changes
# (cmd/cryo-agent/messages_windows_test.go checks every documented id).
#
# Exit codes: 0 written, 2 a tool is missing, 4 a step failed.

$WindMC  = $env:CRYO_WINDMC    # optional: windmc to use; default windmc on PATH
$WindRes = $env:CRYO_WINDRES   # optional: windres to use; default windres on PATH
$Out     = $env:CRYO_MSG_OUT   # optional: output .syso; default <repo>\cmd\cryo-agent\messages_windows_amd64.syso

$ErrorActionPreference = 'Continue'
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..\..')).Path
$Source   = Join-Path $PSScriptRoot 'messages.mc'
if (-not $WindMC)  { $WindMC = 'windmc' }
if (-not $WindRes) { $WindRes = 'windres' }
if (-not $Out)     { $Out = Join-Path $RepoRoot 'cmd\cryo-agent\messages_windows_amd64.syso' }

function Say($m) { Write-Host "messages: $m" }
function Die($code, $m) { Write-Host "messages: FAILED: $m"; exit $code }

# ---- discovery ---------------------------------------------------------------
foreach ($tool in @($WindMC, $WindRes)) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { Die 2 "$tool is not on PATH (MinGW-w64 binutils; set CRYO_WINDMC / CRYO_WINDRES)" }
}
if (-not (Test-Path -LiteralPath $Source -PathType Leaf)) { Die 2 "$Source is missing" }

# ---- act ---------------------------------------------------------------------
$work = Join-Path ([IO.Path]::GetTempPath()) ('cryo-messages-' + [guid]::NewGuid().ToString('N'))
try {
    New-Item -ItemType Directory -Path $work -ErrorAction Stop | Out-Null
    Copy-Item -LiteralPath $Source -Destination (Join-Path $work 'messages.mc') -ErrorAction Stop
} catch { Die 4 "the work folder could not be prepared: $($_.Exception.Message)" }

Push-Location $work
try {
    # No severity, facility or customer bits: the ids are exactly the event
    # ids ReportEvent writes (golang.org/x/sys/windows/svc/eventlog passes the
    # bare id), which is what Event Viewer looks up.
    & $WindMC -h . -r . messages.mc
    $rc = $LASTEXITCODE
    if ($rc -eq 0) {
        & $WindRes --target=pe-x86-64 -O coff -i messages.rc -o messages.syso
        $rc = $LASTEXITCODE
    }
} finally { Pop-Location }
if ($rc -ne 0 -or -not (Test-Path -LiteralPath (Join-Path $work 'messages.syso'))) {
    Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
    Die 4 'windmc or windres failed'
}
try { Copy-Item -LiteralPath (Join-Path $work 'messages.syso') -Destination $Out -Force -ErrorAction Stop }
catch { Die 4 "the .syso could not be written: $($_.Exception.Message)" }
Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue

# ---- verify / report ---------------------------------------------------------
$len = (Get-Item -LiteralPath $Out).Length
if ($len -le 0) { Die 4 "$Out is empty" }
Say "wrote $Out ($len bytes, SHA-256 $((Get-FileHash -LiteralPath $Out -Algorithm SHA256).Hash.ToLowerInvariant()))"
exit 0
