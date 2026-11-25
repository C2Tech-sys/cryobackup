# 07-browse.ps1 - item 7: browse latency of `restic ls --json --no-cache <snap>
# <dir>` (one level, cold, no cache) through the gateway and through restic's
# rclone: backend, with the current snapshot count and again with 30 snapshots.
# LOCAL DISK ONLY: object-storage latency is NOT measured by this rig.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$Snap = Get-FullSnap
$Target = 30            # snapshots in the repository for the second round
$Repeat = 3             # calls per measurement
$ChangePerSnap = 50     # files touched before each extra backup
$e = Get-GatewayEnv
$ea = Get-AdminEnv
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 7 start (local disk; object-storage latency NOT measured; this PC was at 100 % CPU from other work during the run)'

# snapshot objects in the device repository, counted on disk behind the gateway
# (PowerShell 5.1's ConvertFrom-Json hands a JSON array down the pipeline as ONE object)
function Get-SnapCount { @(Get-ChildItem -LiteralPath (Join-Path $RepoTop "$DeviceId\snapshots") -File).Count }

function Measure-Round([string]$Tag) {
    $snaps = Get-SnapCount
    $idx = @(Get-ChildItem -LiteralPath (Join-Path $RepoTop "$DeviceId\index") -File).Count
    $packs = @(Get-ChildItem -LiteralPath (Join-Path $RepoTop "$DeviceId\data") -File -Recurse).Count
    Write-Note "item7 $Tag : snapshots=$snaps index files=$idx pack files=$packs"
    $dirs = @(@('top /data (9 entries)', "/$L/data"), @('flat (2000 entries)', "/$L/data/many/flat"), @('d01 (1000 entries)', "/$L/data/many/d01"))
    foreach ($path in @(@('gateway', $e, @('--no-cache')), @('rclone:', $ea, (Get-AdminOpts)))) {
        $base = @()
        for ($i = 0; $i -lt $Repeat; $i++) { $base += (Invoke-Rig -Label "item7 $Tag $($path[0]) cat config" -Exe $Restic -ArgList ($path[2] + @('cat', 'config')) -Env $path[1] -NoLog).Ms }
        Write-Note ("item7 {0} {1} baseline 'cat config' (process start + key + config) ms: {2}" -f $Tag, $path[0], ($base -join ' '))
        foreach ($d in $dirs) {
            $ms = @(); $lines = 0; $rc = 0
            for ($i = 0; $i -lt $Repeat; $i++) {
                $r = Invoke-Rig -Label "item7 $Tag $($path[0]) ls $($d[0])" -Exe $Restic -ArgList ($path[2] + @('ls', '--json', $Snap, $d[1])) -Env $path[1] -NoLog:($i -gt 0)
                $ms += $r.Ms; $rc = $r.Rc; $lines = @($r.Out -split "`n" | Where-Object { $_ -match '"struct_type":"node"' }).Count
            }
            Write-Note ("item7 {0} {1} ls --json --no-cache {2}: exit {3}, nodes {4}, ms: {5}" -f $Tag, $path[0], $d[0], $rc, $lines, ($ms -join ' '))
        }
    }
}

if ($env:P22_ITEM7_SKIP_ROUND1 -ne '1') { Measure-Round 'round1' }

# add snapshots until the repository holds $Target. Each extra backup covers a
# SMALL folder with $ChangePerSnap changed files: a full backup of the 20 000
# files took about 10 minutes per run under this PC's load. Every backup run
# writes its own index file and pack(s), which is what an uncached ls has to load.
$have = Get-SnapCount
$k = 0
while ($have -lt $Target) {
    $k++
    for ($i = 0; $i -lt $ChangePerSnap; $i++) { [IO.File]::WriteAllText(("$DriveDir\data\nested\churn{0:D3}.txt" -f $i), "round $k file $i", $Utf8NoBom) }
    $r = Invoke-Rig -Label "item7 extra backup $k" -Exe $Restic -ArgList @('backup', '--host', $DeviceId, '--tag', 'browse', '--json', '--', "$drive\data\nested") -Env $e -NoLog:($k -gt 1)
    Write-Host "extra backup $k exit $($r.Rc) ms $($r.Ms)"
    if ($r.Rc -ne 0) { throw "extra backup $k failed" }
    $have = Get-SnapCount
}
Write-Note "item7 added $k snapshots (each: $ChangePerSnap changed files in a small folder, backup of that folder)"
Measure-Round 'round2'
