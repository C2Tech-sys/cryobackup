# rig.ps1 - shared variables and helpers of the P22-S1 spike rig (dot-sourced by
# every step script; never run on its own). Windows PowerShell 5.1, no elevation.
#
# The rig is throwaway: a local-directory rclone remote, `rclone serve restic`
# with the gateway flags of deploy/rclone/gateway-flags.md on a loopback port,
# one device login, one restic repository, sample data on a subst drive. The
# passwords are random, made by 00-setup.ps1, kept in files under $Root\secrets
# and handed to restic through the environment only (never argv, never printed).
#
# Every call goes through Invoke-Rig, which appends the command line (rig paths
# made generic), exit code, wall time and the output to $Root\log\calls.md.

# ---- variables (override through the environment) --------------------------
$Root     = if ($env:P22_RIG_ROOT) { $env:P22_RIG_ROOT } else { Join-Path $env:TEMP 'cryo-p22-rig' }
$Restic   = if ($env:CRYO_RESTIC) { $env:CRYO_RESTIC } else { (Get-Command restic -ErrorAction Stop).Source }
$Rclone   = if ($env:CRYO_RCLONE) { $env:CRYO_RCLONE } else { (Get-Command rclone -ErrorAction Stop).Source }
$Python   = if ($env:P22_PYTHON) { $env:P22_PYTHON } else { (Get-Command python -ErrorAction SilentlyContinue).Source }
$Port     = if ($env:P22_PORT) { [int]$env:P22_PORT } else { 19443 }
$DeviceId = 'spike-dev-01'          # the device login == its repository folder
$Bucket   = 'bucket'                # folder standing in for the bucket
$Prefix   = 'cryo'                  # folder standing in for the gateway prefix
$RepoTop  = Join-Path $Root "store\$Bucket\$Prefix"   # what the gateway serves
$DriveDir = Join-Path $Root 'drive'                     # the subst drive's root
$RepoDir  = $PSScriptRoot | Split-Path | Split-Path | Split-Path  # repository root (for go run)

# ---- derived -----------------------------------------------------------------
$Secrets = Join-Path $Root 'secrets'
$LogDir  = Join-Path $Root 'log'
$OutDir  = Join-Path $Root 'out'
$State   = Join-Path $Root 'state'
foreach ($d in @($Root, $Secrets, $LogDir, $OutDir, $State)) {
    if (-not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
}
$CallLog = Join-Path $LogDir 'calls.md'
$Utf8NoBom = New-Object System.Text.UTF8Encoding($false)

function Get-RigDrive {
    $f = Join-Path $State 'drive.txt'
    if (Test-Path -LiteralPath $f) { return (Get-Content -LiteralPath $f -TotalCount 1).Trim() }
    return $null
}
# The full-data snapshot made by 00-setup.ps1 (later items address it by id, not
# "latest", because item 1 and item 7 add snapshots).
function Get-FullSnap {
    $f = Join-Path $State 'snap-full.txt'
    if (-not (Test-Path -LiteralPath $f)) { throw 'state\snap-full.txt missing: run 00-setup.ps1' }
    return (Get-Content -LiteralPath $f -TotalCount 1).Trim()
}
function Get-Secret([string]$Name) {
    $f = Join-Path $Secrets $Name
    if (-not (Test-Path -LiteralPath $f)) { throw "secret $Name missing: run 00-setup.ps1" }
    return ([IO.File]::ReadAllText($f)).Trim()
}

# Generic: replace rig paths and binary paths so logs can be quoted in docs.
function ConvertTo-Generic([string]$s) {
    if ($null -eq $s) { return $s }
    $s = $s.Replace($Restic, 'restic').Replace($Rclone, 'rclone')
    if ($Python) { $s = $s.Replace($Python, 'python') }
    $s = $s.Replace($Root, '<rig>').Replace(($Root -replace '\\', '/'), '<rig>')
    foreach ($n in @('gateway-password', 'repo-password', 'self-password')) {
        $f = Join-Path $Secrets $n
        if (Test-Path -LiteralPath $f) { $v = ([IO.File]::ReadAllText($f)).Trim(); if ($v) { $s = $s.Replace($v, "<$n>") } }
    }
    $s = $s -replace [regex]::Escape($env:USERNAME), '<user>'
    $s = $s -replace [regex]::Escape($env:COMPUTERNAME), '<pc>'
    return $s
}

# Quote one argument by the CommandLineToArgvW rules.
function ConvertTo-ArgString([string[]]$a) {
    $parts = foreach ($x in $a) {
        if ($x -eq '') { '""' }
        elseif ($x -notmatch '[\s"]') { $x }
        else { '"' + (($x -replace '(\\*)"', '$1$1\"') -replace '(\\+)$', '$1$1') + '"' }
    }
    return ($parts -join ' ')
}

# Environment for restic through the gateway with the device login.
function Get-GatewayEnv {
    $pw = Get-Secret 'gateway-password'
    @{
        RESTIC_REPOSITORY = "rest:https://${DeviceId}:${pw}@127.0.0.1:${Port}/${DeviceId}/"
        RESTIC_PASSWORD   = (Get-Secret 'repo-password')
        RESTIC_CACERT     = (Join-Path $Secrets 'gateway.crt')
        RESTIC_CACHE_DIR  = (Join-Path $Root 'cache-device')
        TEMP              = (Join-Path $Root 'tmp'); TMP = (Join-Path $Root 'tmp')
    }
}
# Environment for restic through restic's rclone: backend, the maintain worker's
# admin path (internal/restic/env.go AdminRepository/AdminOptions; --no-cache on
# the command line, no RESTIC_CACHE_DIR).
function Get-AdminEnv([string]$Repo = $DeviceId, [string]$PasswordName = 'repo-password') {
    @{
        RESTIC_REPOSITORY = "rclone:admin:$(($RepoTop -replace '\\','/'))/$Repo"
        RESTIC_PASSWORD   = (Get-Secret $PasswordName)
        RCLONE_CONFIG     = (Join-Path $Secrets 'rclone.conf')
        RESTIC_CACHE_DIR  = $null
        RESTIC_CACERT     = $null
        TEMP              = (Join-Path $Root 'tmp'); TMP = (Join-Path $Root 'tmp')
    }
}
# The -o options of the maintain worker (restic.AdminOptions); rclone.program
# must carry no backslash or whitespace, so forward slashes.
function Get-AdminOpts { @('--no-cache', '-o', "rclone.program=$($Rclone -replace '\\','/')", '-o', 'rclone.args=serve restic --stdio') }

# Invoke-Rig runs one process, records it, and returns rc / ms / stdout / stderr.
#   -StdoutFile  stdout goes to this file as bytes (for dump); else captured as text
#   -NoLog       do not append to calls.md (polling helpers)
function Invoke-Rig {
    param(
        [Parameter(Mandatory)] [string]$Label,
        [Parameter(Mandatory)] [string]$Exe,
        [string[]]$ArgList = @(),
        [hashtable]$Env = @{},
        [string]$StdoutFile,
        [string]$Cwd,
        [int]$TimeoutSec = 3600,
        [switch]$NoLog
    )
    $h = Start-Rig -Label $Label -Exe $Exe -ArgList $ArgList -Env $Env -StdoutFile $StdoutFile -Cwd $Cwd
    return (Wait-Rig $h -TimeoutSec $TimeoutSec -NoLog:$NoLog)
}

# Start-Rig starts the process and returns a handle; Wait-Rig finishes it.
function Start-Rig {
    param(
        [Parameter(Mandatory)] [string]$Label,
        [Parameter(Mandatory)] [string]$Exe,
        [string[]]$ArgList = @(),
        [hashtable]$Env = @{},
        [string]$StdoutFile,
        [string]$Cwd
    )
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $psi.Arguments = ConvertTo-ArgString $ArgList
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.CreateNoWindow = $true
    if ($Cwd) { $psi.WorkingDirectory = $Cwd }
    foreach ($k in $Env.Keys) {
        if ($null -eq $Env[$k]) { $psi.EnvironmentVariables.Remove($k) } else { $psi.EnvironmentVariables[$k] = [string]$Env[$k] }
    }
    $tmp = Join-Path $Root 'tmp'; if (-not (Test-Path $tmp)) { New-Item -ItemType Directory -Force $tmp | Out-Null }
    $outPath = if ($StdoutFile) { $StdoutFile } else { Join-Path $OutDir ("{0}.stdout" -f ($Label -replace '[^A-Za-z0-9._-]', '_')) }
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $p = [Diagnostics.Process]::Start($psi)
    $fs = [IO.File]::Create($outPath)
    $copy = $p.StandardOutput.BaseStream.CopyToAsync($fs)
    $errTask = $p.StandardError.ReadToEndAsync()
    return [pscustomobject]@{ Label = $Label; Exe = $Exe; Psi = $psi; P = $p; Fs = $fs; Copy = $copy; ErrTask = $errTask; Sw = $sw; OutPath = $outPath; StdoutFile = $StdoutFile }
}
function Wait-Rig {
    param([Parameter(Mandatory)] $H, [int]$TimeoutSec = 3600, [switch]$NoLog)
    $p = $H.P; $psi = $H.Psi; $fs = $H.Fs; $copy = $H.Copy; $errTask = $H.ErrTask; $sw = $H.Sw
    $Label = $H.Label; $Exe = $H.Exe; $outPath = $H.OutPath; $StdoutFile = $H.StdoutFile
    $done = $p.WaitForExit($TimeoutSec * 1000)
    if (-not $done) { try { $p.Kill() } catch {} ; $p.WaitForExit() }
    $p.WaitForExit()
    $copy.Wait(); $fs.Close()
    $sw.Stop()
    $rc = if ($done) { $p.ExitCode } else { -999 }
    $err = $errTask.Result
    $out = if ($StdoutFile) { "<binary stdout: $((Get-Item -LiteralPath $outPath).Length) bytes to $(ConvertTo-Generic $outPath)>" } else { [IO.File]::ReadAllText($outPath, [Text.Encoding]::UTF8) }
    $r = [pscustomobject]@{ Label = $Label; Rc = $rc; Ms = $sw.ElapsedMilliseconds; Out = $out; Err = $err; Cmd = (ConvertTo-Generic ("$Exe " + $psi.Arguments)); TimedOut = (-not $done) }
    if (-not $NoLog) { Write-RigLog $r }
    return $r
}

function Limit-Text([string]$s, [int]$Head = 40, [int]$Tail = 15) {
    if (-not $s) { return '' }
    $l = $s -split "`r?`n"
    if ($l.Count -le ($Head + $Tail)) { return ($l -join "`n") }
    return (($l[0..($Head - 1)] -join "`n") + "`n... ($($l.Count - $Head - $Tail) lines omitted) ...`n" + ($l[($l.Count - $Tail)..($l.Count - 1)] -join "`n"))
}

function Write-RigLog($r) {
    $t = "`n### $($r.Label)`n`n``````text`n$ $($r.Cmd)`nexit=$($r.Rc) ms=$($r.Ms)$(if ($r.TimedOut) {' TIMEOUT'})`n--- stdout`n$(ConvertTo-Generic (Limit-Text $r.Out))`n--- stderr`n$(ConvertTo-Generic (Limit-Text $r.Err))`n```````n"
    [IO.File]::AppendAllText($CallLog, $t, $Utf8NoBom)
}
function Write-Note([string]$Text) {
    [IO.File]::AppendAllText($CallLog, "`n> NOTE: $(ConvertTo-Generic $Text)`n", $Utf8NoBom)
    Write-Host (ConvertTo-Generic $Text)
}
function Show-Result($r) { Write-Host ("[{0}] exit={1} ms={2}" -f $r.Label, $r.Rc, $r.Ms) }

function Invoke-Restic([string]$Label, [string[]]$ArgList, [hashtable]$Env, [string]$StdoutFile, [int]$TimeoutSec = 3600) {
    $r = Invoke-Rig -Label $Label -Exe $Restic -ArgList $ArgList -Env $Env -StdoutFile $StdoutFile -TimeoutSec $TimeoutSec
    Show-Result $r
    return $r
}

# ---- gateway -------------------------------------------------------------------
function Get-GatewayArgs {
    @('serve', 'restic',
      '--addr', "127.0.0.1:$Port",
      '--cert', (Join-Path $Secrets 'gateway.crt'), '--key', (Join-Path $Secrets 'gateway.key'),
      '--min-tls-version', 'tls1.2',
      '--htpasswd', (Join-Path $Secrets 'htpasswd'),
      '--append-only',
      '--private-repos',
      '-vv', '--log-file', (Join-Path $LogDir 'gateway.log'),
      "store:$(($RepoTop -replace '\\','/'))")
}
function Test-Port([int]$P) {
    $c = New-Object Net.Sockets.TcpClient
    try { $c.Connect('127.0.0.1', $P); return $true } catch { return $false } finally { $c.Close() }
}
function Start-Gateway {
    if (Test-Port $Port) { throw "port $Port already has a listener" }
    $env:RCLONE_CONFIG = Join-Path $Secrets 'rclone.conf'
    $a = Get-GatewayArgs
    [IO.File]::AppendAllText($CallLog, "`n### gateway start`n`n``````text`n$ $(ConvertTo-Generic ("$Rclone " + (ConvertTo-ArgString $a)))`n```````n", $Utf8NoBom)
    $p = Start-Process -FilePath $Rclone -ArgumentList (ConvertTo-ArgString $a) -PassThru -WindowStyle Hidden
    Remove-Item Env:RCLONE_CONFIG
    Set-Content -LiteralPath (Join-Path $State 'gateway.pid') -Value $p.Id
    for ($i = 0; $i -lt 100; $i++) { if (Test-Port $Port) { return $p } ; Start-Sleep -Milliseconds 100 }
    throw 'gateway did not start listening'
}
function Stop-Gateway {
    $f = Join-Path $State 'gateway.pid'
    if (Test-Path -LiteralPath $f) {
        $id = [int](Get-Content -LiteralPath $f -TotalCount 1)
        $p = Get-Process -Id $id -ErrorAction SilentlyContinue
        if ($p -and $p.ProcessName -like 'rclone*') { Stop-Process -Id $id -Force; $p.WaitForExit(10000) | Out-Null }
        Remove-Item -LiteralPath $f -Force
    }
    for ($i = 0; $i -lt 50 -and (Test-Port $Port); $i++) { Start-Sleep -Milliseconds 100 }
}

# Lock objects currently in the device repository (through the filesystem
# behind the gateway, never through the gateway).
function Get-LockCount { $d = Join-Path $RepoTop "$DeviceId\locks"; if (Test-Path -LiteralPath $d) { @(Get-ChildItem -LiteralPath $d -File).Count } else { 0 } }

# Directory ACL / time snapshot for before/after comparisons.
function Get-Meta([string]$Path) {
    $i = Get-Item -LiteralPath $Path -Force
    $acl = (& icacls.exe $Path 2>&1 | Out-String).Trim()
    [pscustomobject]@{ Path = $Path; LastWriteUtc = $i.LastWriteTimeUtc.ToString('o'); CreationUtc = $i.CreationTimeUtc.ToString('o'); Acl = $acl }
}
