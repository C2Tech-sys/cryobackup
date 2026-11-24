# 06-dump.ps1 - item 6: restic dump -a tar / -a zip of a folder: entry names,
# streaming into a PIPE (the writer cannot seek), non-regular entries, empty
# folders, long names; several paths in one dump; exit code and partial output
# when the repository becomes unreachable mid-stream (gateway killed; and the
# maintain path's rclone child killed).
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$Snap = Get-FullSnap
$Bash = if ($env:P22_BASH) { $env:P22_BASH } else { 'C:\Program Files\Git\bin\bash.exe' }
$KillAfterBytes = 50MB          # kill the gateway once this much has arrived
$GiveUpSec = 1200               # stop waiting for restic after the kill
$e = Get-GatewayEnv
$e.PYTHONIOENCODING = 'utf-8'
$e.MSYS_NO_PATHCONV = '1'; $e.MSYS2_ARG_CONV_EXCL = '*'   # keep /R/data/... as restic sees it
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 6 start'
$lister = Join-Path $PSScriptRoot 'archive_list.py'

# restic ... | python archive_list.py <kind>, in Git Bash; prints both exit codes.
function Invoke-Pipe([string]$Label, [string]$Kind, [string[]]$ResticArgs, [hashtable]$Env = $e) {
    $script = 'r="$1"; p="$2"; l="$3"; k="$4"; shift 4; "$r" "$@" | "$p" "$l" "$k"; echo "pipestatus restic=${PIPESTATUS[0]} reader=${PIPESTATUS[1]}"'
    $a = @('-c', $script, '_', ($Restic -replace '\\', '/'), ($Python -replace '\\', '/'), ($lister -replace '\\', '/'), $Kind) + $ResticArgs
    $r = Invoke-Rig -Label "item6 $Label" -Exe $Bash -ArgList $a -Env $Env
    Show-Result $r
    return $r
}

# python archive_list.py tar < <file>
function Read-Partial([string]$Label, [string]$File) {
    $a = @('-c', '"$1" "$2" tar < "$3"', '_', ($Python -replace '\\', '/'), ($lister -replace '\\', '/'), ($File -replace '\\', '/'))
    return (Invoke-Rig -Label $Label -Exe $Bash -ArgList $a -Env $e)
}

if ($env:P22_ITEM6_SKIP_ARCHIVES -ne '1') {
Invoke-Pipe 'tar names (special + non-ASCII)' 'tar' @('dump', '-a', 'tar', $Snap, "/$L/data/names") | Out-Null
Invoke-Pipe 'tar long path folder' 'tar' @('dump', '-a', 'tar', $Snap, "/$L/data/long") | Out-Null
Invoke-Pipe 'tar empty folder' 'tar' @('dump', '-a', 'tar', $Snap, "/$L/data/empty") | Out-Null
Invoke-Pipe 'tar nested (sub-folders)' 'tar' @('dump', '-a', 'tar', $Snap, "/$L/data/nested") | Out-Null
Invoke-Pipe 'tar acl + ads folders' 'tar' @('dump', '-a', 'tar', $Snap, "/$L/data/ads") | Out-Null
Invoke-Pipe 'zip names' 'zip' @('dump', '-a', 'zip', $Snap, "/$L/data/names") | Out-Null
Invoke-Pipe 'zip long path folder' 'zip' @('dump', '-a', 'zip', $Snap, "/$L/data/long") | Out-Null
Invoke-Pipe 'zip empty folder' 'zip' @('dump', '-a', 'zip', $Snap, "/$L/data/empty") | Out-Null
Invoke-Pipe 'zip nested' 'zip' @('dump', '-a', 'zip', $Snap, "/$L/data/nested") | Out-Null
$r = Invoke-Restic 'item6 dump -a tar of a single FILE' @('dump', '-a', 'tar', $Snap, "/$L/data/nested/top.txt") $e
Write-Note "item6 single file with -a tar: stdout bytes=$($r.Out.Length) starts '$($r.Out.Substring(0, [Math]::Min(20, $r.Out.Length)))'"
$r = Invoke-Restic 'item6 dump with TWO paths' @('dump', '-a', 'tar', $Snap, "/$L/data/nested", "/$L/data/names") $e
$r = Invoke-Restic 'item6 dump of snapshot root /' @('dump', '-a', 'tar', $Snap, '/') $e -StdoutFile (Join-Path $Root 'tmp\root.tar') -TimeoutSec 60
Write-Note "item6 note: the root dump above was capped at 60 s (it includes the 1 GiB file); only its start matters"
}   # P22_ITEM6_SKIP_ARCHIVES=1 starts here (re-runs of the non-regular and outage parts)

# non-regular entries: junction, hardlink, file symlink (symlink needs privilege or Developer Mode)
$nr = Join-Path $DriveDir 'data\nonreg'
if (-not (Test-Path -LiteralPath $nr)) {
    New-Item -ItemType Directory -Force -Path $nr | Out-Null
    [IO.File]::WriteAllText("$nr\target.txt", 'target', $Utf8NoBom)
    # mklink writes failures to stderr; with ErrorActionPreference=Stop that would end the script
    $ErrorActionPreference = 'Continue'
    & cmd.exe /c mklink /J "$nr\junction" (Join-Path $DriveDir 'data\nested') *> $null
    & cmd.exe /c mklink /H "$nr\hardlink.txt" "$nr\target.txt" *> $null
    & cmd.exe /c mklink "$nr\symlink.txt" "target.txt" *> $null
    $ErrorActionPreference = 'Stop'
}
Write-Note ("item6 non-regular folder holds: {0}" -f ((Get-ChildItem -LiteralPath $nr -Force | ForEach-Object { "$($_.Name) [$($_.Attributes)]" }) -join '; '))
Write-Note "item6 (a file symlink needs SeCreateSymbolicLinkPrivilege or Developer Mode; absent above means mklink was refused)"
$r = Invoke-Restic 'item6 backup non-regular folder (tag nonreg)' @('backup', '--host', $DeviceId, '--tag', 'nonreg', '--json', '--', "$drive\data\nonreg") $e
$r = Invoke-Restic 'item6 ls non-regular snapshot' @('ls', '--json', '--tag', 'nonreg', 'latest', "/$L/data/nonreg") $e
Invoke-Pipe 'tar non-regular folder' 'tar' @('dump', '-a', 'tar', '--tag', 'nonreg', 'latest', "/$L/data/nonreg") | Out-Null
Invoke-Pipe 'zip non-regular folder' 'zip' @('dump', '-a', 'zip', '--tag', 'nonreg', 'latest', "/$L/data/nonreg") | Out-Null

# unreachable mid-stream: gateway killed while dump -a tar of the 1 GiB folder streams
$part = Join-Path $Root 'tmp\partial-gateway.tar'
$h = Start-Rig -Label 'item6 dump -a tar big folder, gateway killed mid-stream' -Exe $Restic -ArgList @('dump', '-a', 'tar', $Snap, "/$L/data/big") -Env $e -StdoutFile $part
while (-not $h.P.HasExited -and (Get-Item -LiteralPath $part).Length -lt $KillAfterBytes) { Start-Sleep -Milliseconds 100 }
$killedAt = (Get-Item -LiteralPath $part).Length
$sw = [Diagnostics.Stopwatch]::StartNew()
Stop-Gateway
Write-Note "item6 gateway killed after $killedAt bytes; listener gone: $(-not (Test-Port $Port))"
$sizes = @()
while (-not $h.P.HasExited -and $sw.Elapsed.TotalSeconds -lt $GiveUpSec) { Start-Sleep -Seconds 5; if ($sizes.Count -lt 400) { $sizes += (Get-Item -LiteralPath $part).Length } }
$exitedAlone = $h.P.HasExited
$r = Wait-Rig $h -TimeoutSec 1; Show-Result $r
Write-Note "item6 after kill: restic exited by itself=$exitedAlone after $([int]$sw.Elapsed.TotalSeconds) s; exit=$($r.Rc); final bytes=$((Get-Item -LiteralPath $part).Length); growth after kill: $(($sizes | Select-Object -Unique) -join ' ')"
$pr = Read-Partial 'item6 read the partial tar' $part
Show-Result $pr
Start-Gateway | Out-Null

# maintain path: rclone: backend, kill the rclone child restic started
$ea = Get-AdminEnv
$part2 = Join-Path $Root 'tmp\partial-admin.tar'
$h = Start-Rig -Label 'item6 dump -a tar big folder via rclone: backend, rclone child killed' -Exe $Restic -ArgList ((Get-AdminOpts) + @('dump', '-a', 'tar', $Snap, "/$L/data/big")) -Env $ea -StdoutFile $part2
while (-not $h.P.HasExited -and (Get-Item -LiteralPath $part2).Length -lt $KillAfterBytes) { Start-Sleep -Milliseconds 100 }
$kids = @(Get-CimInstance Win32_Process -Filter "ParentProcessId=$($h.P.Id)" | Where-Object { $_.Name -like 'rclone*' })
$at2 = (Get-Item -LiteralPath $part2).Length
$sw = [Diagnostics.Stopwatch]::StartNew()
foreach ($k in $kids) { Stop-Process -Id $k.ProcessId -Force }
Write-Note "item6 killed $($kids.Count) rclone child(ren) of restic after $at2 bytes"
while (-not $h.P.HasExited -and $sw.Elapsed.TotalSeconds -lt $GiveUpSec) { Start-Sleep -Seconds 2 }
$exitedAlone = $h.P.HasExited
$r = Wait-Rig $h -TimeoutSec 1; Show-Result $r
Write-Note "item6 rclone: path after kill: restic exited by itself=$exitedAlone after $([int]$sw.Elapsed.TotalSeconds) s; exit=$($r.Rc); final bytes=$((Get-Item -LiteralPath $part2).Length)"
$pr = Read-Partial 'item6 read the partial tar (rclone: path)' $part2
Show-Result $pr
Remove-Item -LiteralPath $part, $part2, (Join-Path $Root 'tmp\root.tar') -Force -ErrorAction SilentlyContinue
