# 04-overwrite.ps1 - item 4: --overwrite never | if-changed | if-newer | always:
# what the --json summary reports, and a target file held open with a share-deny
# lock by another process.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$Snap = Get-FullSnap
$e = Get-GatewayEnv
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 4 start'
$t = Join-Path $Root 'restore\item4'
if (Test-Path -LiteralPath $t) { Remove-Item -LiteralPath $t -Recurse -Force }
$inc = "/$L/data/names"
$f = Join-Path $t "$L\data\names\a.txt"

function Get-Summary($r) {
    $s = ($r.Out -split "`r?`n" | Where-Object { $_ -match '"message_type":"summary"' } | Select-Object -Last 1)
    if (-not $s) { $s = '(no summary line)' }
    return $s
}
function Restore([string]$Label, [string[]]$Extra) {
    $r = Invoke-Restic "item4 $Label" (@('restore', '--json', $Snap, '--target', $t, '--include', $inc) + $Extra) $e
    Write-Note "item4 $Label -> exit $($r.Rc); summary: $(Get-Summary $r)"
    $errs = ($r.Out -split "`r?`n" | Where-Object { $_ -match '"message_type":"error"' } | Select-Object -First 3) -join ' || '
    if ($errs) { Write-Note "item4 $Label error lines: $errs" }
    return $r
}

Restore 'first restore into empty target (default overwrite)' @() | Out-Null
Restore 'again, --overwrite never' @('--overwrite', 'never') | Out-Null
Restore 'again, --overwrite if-changed (nothing changed)' @('--overwrite', 'if-changed') | Out-Null
Restore 'again, --overwrite always (nothing changed)' @('--overwrite', 'always') | Out-Null
[IO.File]::WriteAllText($f, 'changed content, longer than before', $Utf8NoBom)
Restore 'one file changed, --overwrite never' @('--overwrite', 'never') | Out-Null
Write-Note "item4 a.txt after never: '$([IO.File]::ReadAllText($f))'"
Restore 'one file changed, --overwrite if-changed' @('--overwrite', 'if-changed') | Out-Null
Write-Note "item4 a.txt after if-changed: '$([IO.File]::ReadAllText($f))'"
[IO.File]::WriteAllText($f, 'name: a.tx!', $Utf8NoBom)   # same size, different bytes
(Get-Item -LiteralPath $f).LastWriteTime = Get-Date '2001-01-01'
Restore 'same size, older mtime, --overwrite if-newer' @('--overwrite', 'if-newer') | Out-Null
Write-Note "item4 a.txt after if-newer: '$([IO.File]::ReadAllText($f))'"
Restore 'same size changed, --overwrite always' @('--overwrite', 'always') | Out-Null
Write-Note "item4 a.txt after always: '$([IO.File]::ReadAllText($f))'"

# share-deny lock held by another process
[IO.File]::WriteAllText($f, 'locked and different', $Utf8NoBom)
$cmd = "`$h=[IO.File]::Open('$f','Open','ReadWrite','None'); Start-Sleep -Seconds 300"
$locker = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile', '-Command', $cmd) -PassThru -WindowStyle Hidden
Start-Sleep -Seconds 3
$canOpen = $true; try { [IO.File]::Open($f, 'Open', 'Read', 'ReadWrite').Close() } catch { $canOpen = $false }
Write-Note "item4 lock in place (open for read by us fails): $(-not $canOpen)"
Restore 'locked target, --overwrite always' @('--overwrite', 'always') | Out-Null
Restore 'locked target, --overwrite if-changed' @('--overwrite', 'if-changed') | Out-Null
Restore 'locked target, --overwrite never' @('--overwrite', 'never') | Out-Null
Stop-Process -Id $locker.Id -Force
Start-Sleep -Seconds 1
Write-Note "item4 a.txt after lock released: '$([IO.File]::ReadAllText($f))'"
Restore 'after unlock, --overwrite if-changed' @('--overwrite', 'if-changed') | Out-Null
