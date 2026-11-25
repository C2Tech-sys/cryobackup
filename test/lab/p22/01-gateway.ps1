# 01-gateway.ps1 - item 1: backup, snapshots, ls, dump and restore THROUGH the
# append-only gateway with the device login; lock objects during and after each
# call; 403s in the gateway log; --private-repos confinement of a second login.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive
$Snap = Get-FullSnap
$e = Get-GatewayEnv
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note "ITEM 1 start; lock objects before: $(Get-LockCount)"

# backup --force of 4 000 small files (re-reads them: a window for lock sampling);
# tagged so it never stands in for the full snapshot
$h = Start-Rig -Label 'item1 backup --force (gateway)' -Exe $Restic -ArgList @('backup', '--host', $DeviceId, '--tag', 'item1', '--force', '--json', '--',
    "$drive\data\many\d01", "$drive\data\many\d02", "$drive\data\many\d03", "$drive\data\many\d04") -Env $e
$max = 0; $samples = 0
while (-not $h.P.HasExited) { $c = Get-LockCount; if ($c -gt $max) { $max = $c }; $samples++; Start-Sleep -Milliseconds 50 }
$r = Wait-Rig $h; Show-Result $r
Write-Note "item1 backup: max lock objects seen while running = $max ($samples samples); after exit = $(Get-LockCount)"

$r = Invoke-Restic 'item1 snapshots (gateway)' @('snapshots', '--json') $e
Write-Note "item1 locks after snapshots = $(Get-LockCount)"
$r = Invoke-Restic 'item1 ls (gateway)' @('ls', $Snap, "/$($drive.TrimEnd(':'))/data/nested") $e
$r = Invoke-Restic 'item1 dump (gateway)' @('dump', $Snap, "/$($drive.TrimEnd(':'))/data/nested/top.txt") $e
Write-Note "item1 dump content = '$($r.Out)'"

$tgt = Join-Path $Root 'restore\item1'
if (Test-Path -LiteralPath $tgt) { Remove-Item -LiteralPath $tgt -Recurse -Force }
$h = Start-Rig -Label 'item1 restore (gateway)' -Exe $Restic -ArgList @('restore', '--json', $Snap, '--target', $tgt, '--include', "/$($drive.TrimEnd(':'))/data/nested") -Env $e
$max = 0
while (-not $h.P.HasExited) { $c = Get-LockCount; if ($c -gt $max) { $max = $c }; Start-Sleep -Milliseconds 20 }
$r = Wait-Rig $h; Show-Result $r
$files = @(Get-ChildItem -LiteralPath $tgt -Recurse -File | ForEach-Object { $_.FullName.Substring($tgt.Length) })
Write-Note "item1 restore: max locks while running = $max; after = $(Get-LockCount); files restored: $($files -join ', ')"

# append-only still holds for this login (one destructive call; full L-APPEND-1 lives in deploy/rclone)
$r = Invoke-Restic 'item1 forget (must be 403)' @('forget', $Snap) $e

# --private-repos: the second login must not read the first login's repository
$e2 = Get-GatewayEnv
$e2.RESTIC_REPOSITORY = "rest:https://spike-dev-02:$(Get-Secret 'gateway-password-2')@127.0.0.1:${Port}/${DeviceId}/"
$e2.RESTIC_CACHE_DIR = Join-Path $Root 'cache-dev2'
$r = Invoke-Restic 'item1 other login reads this repo (must be 403)' @('cat', 'config') $e2

# gateway log: status codes and lock traffic
$log = Get-Content -LiteralPath (Join-Path $LogDir 'gateway.log')
Write-Note ("item1 gateway.log lines: {0}; containing '403': {1}; containing 'locks': {2}" -f $log.Count, @($log | Select-String -SimpleMatch '403').Count, @($log | Select-String -SimpleMatch 'locks').Count)
$log | Select-String -SimpleMatch '403' | Select-Object -First 8 | ForEach-Object { Write-Note "  log: $($_.Line)" }
$log | Select-String -Pattern 'locks' | Select-Object -First 6 | ForEach-Object { Write-Note "  log: $($_.Line)" }
