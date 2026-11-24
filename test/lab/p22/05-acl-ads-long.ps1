# 05-acl-ads-long.ps1 - item 5: ACL, owner, long path, alternate data stream and
# empty folder after restore, run as the current (non-elevated) user. SYSTEM is
# NOT tested here. Compares SDDL of the source and the restored copy, for a NEW
# side folder and for the original location.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$Snap = Get-FullSnap
$e = Get-GatewayEnv
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 5 start (as the current user, not SYSTEM, not elevated)'
$src = Join-Path $DriveDir 'data'
$t = Join-Path $Root 'restore\item5-new'
if (Test-Path -LiteralPath $t) { Remove-Item -LiteralPath $t -Recurse -Force }
$rd = Join-Path $t "$L\data"

$r = Invoke-Restic 'item5 restore acl/long/ads/empty into a NEW folder' @('restore', '--json', $Snap, '--target', $t,
    '--include', "/$L/data/acl", '--include', "/$L/data/long", '--include', "/$L/data/ads", '--include', "/$L/data/empty") $e
$errs = ($r.Out -split "`r?`n" | Where-Object { $_ -match '"message_type":"error"' } | Select-Object -First 5) -join ' || '
Write-Note "item5 restore exit $($r.Rc); error lines: $(if ($errs) { $errs } else { 'none' })"

function Compare-Sd([string]$Rel) {
    $a = Get-Acl -LiteralPath (Join-Path $src $Rel); $b = Get-Acl -LiteralPath (Join-Path $rd $Rel)
    Write-Note ("item5 {0}: owner src={1} restored={2}; DACL protected src={3} restored={4}; SDDL equal={5}`n  src:      {6}`n  restored: {7}" -f $Rel,
        $a.Owner, $b.Owner, $a.AreAccessRulesProtected, $b.AreAccessRulesProtected, ($a.Sddl -eq $b.Sddl), $a.Sddl, $b.Sddl)
}
foreach ($rel in @('acl\custom.txt', 'acl\customdir', 'acl\customdir\inner.txt', 'acl\plain.txt')) { Compare-Sd $rel }
Write-Note "item5 icacls restored custom.txt:`n$(& icacls.exe (Join-Path $rd 'acl\custom.txt') | Out-String)"
Write-Note "item5 icacls restored plain.txt (inherited-only at source):`n$(& icacls.exe (Join-Path $rd 'acl\plain.txt') | Out-String)"

# long path
$seg = 'L' * 60
$lp = "\\?\$rd\long\$seg\$seg\$seg\$seg\$('f' * 70).txt"
$len = ("$rd\long\$seg\$seg\$seg\$seg\$('f' * 70).txt").Length
Write-Note "item5 long path ($len chars) restored: $([IO.File]::Exists($lp)); content '$(if ([IO.File]::Exists($lp)) { [IO.File]::ReadAllText($lp) })'"

# alternate data stream
$ads = Join-Path $rd 'ads\withstream.txt'
$streams = @(Get-Item -LiteralPath $ads -Stream * | ForEach-Object { "$($_.Stream)($($_.Length))" }) -join ', '
$srcStreams = @(Get-Item -LiteralPath (Join-Path $src 'ads\withstream.txt') -Stream * | ForEach-Object { "$($_.Stream)($($_.Length))" }) -join ', '
Write-Note "item5 ADS source streams: $srcStreams; restored streams: $streams"
Write-Note "item5 empty folder restored: $(Test-Path -LiteralPath (Join-Path $rd 'empty') -PathType Container)"

# original location: delete custom.txt and the ADS file, restore by the drive-root form
Remove-Item -LiteralPath (Join-Path $src 'acl\custom.txt') -Force
Remove-Item -LiteralPath (Join-Path $src 'ads\withstream.txt') -Force
$r = Invoke-Restic 'item5 restore custom.txt + ads into ORIGINAL location' @('restore', '--json', "${Snap}:/$L", '--target', "$drive\",
    '--include', '/data/acl/custom.txt', '--include', '/data/ads/withstream.txt') $e
Write-Note "item5 original-location custom.txt:`n$(& icacls.exe (Join-Path $src 'acl\custom.txt') | Out-String)"
$st = @(Get-Item -LiteralPath (Join-Path $src 'ads\withstream.txt') -Stream * | ForEach-Object { "$($_.Stream)($($_.Length))" }) -join ', '
Write-Note "item5 original-location withstream.txt streams: $st"
