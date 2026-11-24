# 02b-parent-acl.ps1 - item 2, controlled: on a FRESH tree (never restored into),
# what security descriptor restic records for a directory, and what a restore of
# ONE file does to the existing parents on its path and to an untouched sibling.
#   D: drive-root form  <snap>:/<L> --target <L>:\ --include /fresh/p1/p2/f.txt
#   E: parent-folder form <snap>:/<L>/fresh/p1/p2 --target <L>:\fresh\p1\p2 --include /f.txt
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$e = Get-GatewayEnv
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 2b start (fresh tree, SDDL before/after)'
$fresh = Join-Path $DriveDir 'fresh'
if (Test-Path -LiteralPath $fresh) { Remove-Item -LiteralPath $fresh -Recurse -Force }
New-Item -ItemType Directory -Force -Path "$fresh\p1\p2", "$fresh\other" | Out-Null
[IO.File]::WriteAllText("$fresh\p1\p2\f.txt", 'f', $Utf8NoBom)
[IO.File]::WriteAllText("$fresh\p1\sib.txt", 'sibling', $Utf8NoBom)
[IO.File]::WriteAllText("$fresh\other\o.txt", 'other branch', $Utf8NoBom)
$watch = [ordered]@{ 'drive root' = $DriveDir; 'fresh' = $fresh; 'p1' = "$fresh\p1"; 'p2' = "$fresh\p1\p2"; 'p2\f.txt' = "$fresh\p1\p2\f.txt"; 'p1\sib.txt' = "$fresh\p1\sib.txt"; 'other\o.txt' = "$fresh\other\o.txt" }
function Show-Sd([string]$Tag) {
    foreach ($k in $watch.Keys) {
        $p = $watch[$k]
        if (Test-Path -LiteralPath $p) { $i = Get-Item -LiteralPath $p -Force; Write-Note ("item2b {0} {1}: mtime={2} sddl={3}" -f $Tag, $k, $i.LastWriteTimeUtc.ToString('o'), ((Get-Acl -LiteralPath $p).Sddl -replace '^O:[^G]+G:\S+?D:', 'D:')) }
        else { Write-Note "item2b $Tag ${k}: (absent)" }
    }
}
Show-Sd 'created'
$r = Invoke-Restic 'item2b backup fresh tree (tag fresh)' @('backup', '--host', $DeviceId, '--tag', 'fresh', '--json', '--', "$drive\fresh") $e
$snap = (($r.Out -split "`r?`n" | Where-Object { $_ -match '"message_type":"summary"' } | Select-Object -Last 1) | ConvertFrom-Json).snapshot_id
Show-Sd 'after-backup'

# what restic recorded for p1 and f.txt
foreach ($pair in @(@("${snap}:/$L/fresh", 'p1'), @("${snap}:/$L/fresh/p1/p2", 'f.txt'))) {
    $t = Invoke-Rig -Label "item2b cat tree $($pair[0])" -Exe $Restic -ArgList @('cat', 'tree', $pair[0]) -Env $e -NoLog
    if ($t.Rc -ne 0) { Write-Note "item2b cat tree $($pair[0]) exit $($t.Rc): $($t.Err)"; continue }
    $node = ($t.Out | ConvertFrom-Json).nodes | Where-Object { $_.name -eq $pair[1] }
    $ga = $node.generic_attributes
    $b64 = if ($ga) { $ga.'windows.security_descriptor' } else { $null }
    $sddl = if ($b64) { [Security.AccessControl.RawSecurityDescriptor]::new([byte[]][Convert]::FromBase64String($b64), 0).GetSddlForm('Access') } else { '(none recorded)' }
    Write-Note "item2b recorded in snapshot for $($pair[1]): generic_attributes keys=$(if ($ga) { ($ga.PSObject.Properties.Name) -join ',' } else { 'none' }); DACL=$sddl"
}

# D: drive-root form, one file, existing parents changed after the backup
Remove-Item -LiteralPath "$fresh\p1\p2\f.txt" -Force
& icacls.exe "$fresh\p1" /grant '*S-1-5-32-546:(R)' | Out-Null
$old = Get-Date '2001-02-03T04:05:06'
foreach ($p in @($fresh, "$fresh\p1", "$fresh\p1\p2")) { (Get-Item -LiteralPath $p).LastWriteTime = $old }
Show-Sd 'D-before'
$r = Invoke-Restic 'item2b D drive-root form, one file' @('restore', '--json', "${snap}:/$L", '--target', "$drive\", '--include', '/fresh/p1/p2/f.txt') $e
Show-Sd 'D-after'

# E: parent-folder form (subfolder = the file's own folder, target = that folder)
Remove-Item -LiteralPath "$fresh\p1\p2\f.txt" -Force
& icacls.exe "$fresh\p1" /grant '*S-1-5-32-546:(R)' | Out-Null
& icacls.exe "$fresh\p1\p2" /grant '*S-1-5-32-546:(R)' | Out-Null
foreach ($p in @($fresh, "$fresh\p1", "$fresh\p1\p2")) { (Get-Item -LiteralPath $p).LastWriteTime = $old }
Show-Sd 'E-before'
$r = Invoke-Restic 'item2b E parent-folder form, one file' @('restore', '--json', "${snap}:/$L/fresh/p1/p2", '--target', "$drive\fresh\p1\p2", '--include', '/f.txt') $e
Show-Sd 'E-after'

# F: same as E but the file is restored with the subfolder = its grandparent (an intermediate dir p2 exists)
Remove-Item -LiteralPath "$fresh\p1\p2\f.txt" -Force
foreach ($p in @($fresh, "$fresh\p1", "$fresh\p1\p2")) { (Get-Item -LiteralPath $p).LastWriteTime = $old }
Show-Sd 'F-before'
$r = Invoke-Restic 'item2b F grandparent form, one file' @('restore', '--json', "${snap}:/$L/fresh/p1", '--target', "$drive\fresh\p1", '--include', '/p2/f.txt') $e
Show-Sd 'F-after'
