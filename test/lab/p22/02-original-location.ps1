# 02-original-location.ps1 - item 2: the subfolder form <snap>:/<L>/... and the
# drive-root form <snap>:/<L> --target <L>:\ ; are --include patterns relative to
# the subfolder; does restic touch timestamps / ACLs of EXISTING parents.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$Snap = Get-FullSnap
$e = Get-GatewayEnv
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 2 start'

function Get-Restored([string]$Dir) {
    if (-not (Test-Path -LiteralPath $Dir)) { return '(target not created)' }
    $f = @(Get-ChildItem -LiteralPath $Dir -Recurse -File -Force | ForEach-Object { $_.FullName.Substring($Dir.Length) })
    if ($f.Count -eq 0) { return '(no files)' } ; return ($f -join ', ')
}
function Reset-Dir([string]$Dir) { if (Test-Path -LiteralPath $Dir) { Remove-Item -LiteralPath $Dir -Recurse -Force } }

# A. subfolder form: which include spelling matches?
$cases = @(
    @('A1 subfolder, include relative to subfolder', "${Snap}:/$L/data/nested", '/a/b'),
    @('A2 subfolder, include as full snapshot path', "${Snap}:/$L/data/nested", "/$L/data/nested/a/b"),
    @('A3 drive-root form, include relative to /L', "${Snap}:/$L", '/data/nested/a/b'),
    @('A4 drive-root form, include as full snapshot path', "${Snap}:/$L", "/$L/data/nested/a/b"),
    @('A5 subfolder, include without leading slash', "${Snap}:/$L/data/nested", 'a/b')
)
$i = 0
foreach ($c in $cases) {
    $i++; $t = Join-Path $Root "restore\item2-a$i"; Reset-Dir $t
    $r = Invoke-Restic "item2 $($c[0])" @('restore', '--json', $c[1], '--target', $t, '--include', $c[2]) $e
    Write-Note "item2 $($c[0]): restored = $(Get-Restored $t)"
}

# B. drive-root form into the ORIGINAL location, with existing parents whose
# metadata we change after the backup (so a re-applied snapshot value would show).
$real = Join-Path $DriveDir 'data\parent\keep'
Remove-Item -LiteralPath "$real\one.txt" -Force
& icacls.exe $real /grant '*S-1-5-32-546:(R)' | Out-Null                 # extra ACE (Guests read) on keep
$old = Get-Date '2001-02-03T04:05:06'
foreach ($p in @($real, (Split-Path $real), (Join-Path $DriveDir 'data'))) { (Get-Item -LiteralPath $p).LastWriteTime = $old }
$watch = @($DriveDir, (Join-Path $DriveDir 'data'), (Join-Path $DriveDir 'data\parent'), $real, "$real\two.txt")
$before = $watch | ForEach-Object { Get-Meta $_ }
$r = Invoke-Restic 'item2 B drive-root form into original location' @('restore', '--json', "${Snap}:/$L", '--target', "$drive\", '--include', '/data/parent/keep/one.txt') $e
$after = $watch | ForEach-Object { Get-Meta $_ }
for ($k = 0; $k -lt $watch.Count; $k++) {
    $b = $before[$k]; $a = $after[$k]
    Write-Note ("item2 B {0}: LastWrite {1} -> {2}; ACL {3}" -f $watch[$k], $b.LastWriteUtc, $a.LastWriteUtc, $(if ($b.Acl -eq $a.Acl) { 'UNCHANGED' } else { "CHANGED`nBEFORE:`n$($b.Acl)`nAFTER:`n$($a.Acl)" }))
}
Write-Note "item2 B one.txt back: $(Test-Path -LiteralPath "$real\one.txt"); content '$(Get-Content -LiteralPath "$real\one.txt" -ErrorAction SilentlyContinue)'"

# C. subfolder form into the original folder (the ticket's <snap>:/C/... --target <dir>)
Remove-Item -LiteralPath "$real\one.txt" -Force
foreach ($p in @($real, (Split-Path $real))) { (Get-Item -LiteralPath $p).LastWriteTime = $old }
$before = $watch | ForEach-Object { Get-Meta $_ }
$r = Invoke-Restic 'item2 C subfolder form into original folder' @('restore', '--json', "${Snap}:/$L/data/parent", '--target', (Join-Path "$drive\" 'data\parent'), '--include', '/keep/one.txt') $e
$after = $watch | ForEach-Object { Get-Meta $_ }
for ($k = 0; $k -lt $watch.Count; $k++) {
    $b = $before[$k]; $a = $after[$k]
    Write-Note ("item2 C {0}: LastWrite {1} -> {2}; ACL {3}" -f $watch[$k], $b.LastWriteUtc, $a.LastWriteUtc, $(if ($b.Acl -eq $a.Acl) { 'UNCHANGED' } else { "CHANGED`nBEFORE:`n$($b.Acl)`nAFTER:`n$($a.Acl)" }))
}
# put keep's ACL back (remove the extra ACE)
& icacls.exe $real /remove:g '*S-1-5-32-546' | Out-Null
