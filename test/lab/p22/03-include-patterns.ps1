# 03-include-patterns.ps1 - item 3: --include / --include-file with names holding
# [ ] $ % ! ; escapes on Windows; $VAR / %VAR% expansion in --include-file; CRLF
# and UTF-8 BOM pattern files. Every case restores into its own side folder.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$drive = Get-RigDrive; $L = $drive.TrimEnd(':')
$Snap = Get-FullSnap
$e = Get-GatewayEnv
$e.P22VAR = 'p22value'          # an environment variable visible to restic
if (-not (Test-Port $Port)) { Start-Gateway | Out-Null }
Write-Note 'ITEM 3 start (P22VAR=p22value in restic''s environment)'
$N = "/$L/data/names"
$uni = 'caf' + [char]0x00E9 + ' ' + [char]0x00FC + [char]0x00F1 + [char]0x00EF + ' ' + [char]0x65E5 + [char]0x672C

$caseNo = 0
function Test-Case([string]$Label, [string[]]$Includes, [string]$FileText, [string]$FileMode = 'lf') {
    $script:caseNo++
    $t = Join-Path $Root "restore\item3-$script:caseNo"
    if (Test-Path -LiteralPath $t) { Remove-Item -LiteralPath $t -Recurse -Force }
    $a = @('restore', '--json', $Snap, '--target', $t)
    foreach ($x in $Includes) { $a += @('--include', $x) }
    if ($null -ne $FileText -and $FileText -ne '') {
        $pf = Join-Path $Root "tmp\item3-$script:caseNo.patterns"
        $txt = if ($FileMode -eq 'crlf' -or $FileMode -eq 'bom-crlf') { $FileText -replace "`n", "`r`n" } else { $FileText }
        $enc = if ($FileMode -like 'bom*') { New-Object System.Text.UTF8Encoding($true) } else { $Utf8NoBom }
        [IO.File]::WriteAllText($pf, $txt, $enc)
        $a += @('--include-file', $pf)
        Write-Note "item3 #$script:caseNo pattern file ($FileMode): $(($FileText -split "`n" | Where-Object { $_ }) -join ' | ')"
    }
    $r = Invoke-Restic "item3 #$script:caseNo $Label" $a $e
    $got = if (Test-Path -LiteralPath $t) { @(Get-ChildItem -LiteralPath $t -Recurse -File -Force | ForEach-Object { $_.Name }) -join ', ' } else { '' }
    if (-not $got) { $got = '(nothing)' }
    Write-Note "item3 #$script:caseNo $Label -> exit $($r.Rc); restored: $got"
}

# --include arguments
Test-Case 'literal [ab] in --include' @("$N/bracket [ab].txt")
Test-Case 'backslash escape \[ab]' @("$N/bracket \[ab].txt")
Test-Case 'class escape [[]ab]' @("$N/bracket [[]ab].txt")
Test-Case 'class escape [[]ab[]]' @("$N/bracket [[]ab[]].txt")
Test-Case 'literal [x].txt' @("$N/[x].txt")
Test-Case 'class escape [[]x].txt' @("$N/[[]x].txt")
Test-Case 'literal x]y.txt' @("$N/x]y.txt")
Test-Case 'literal $P22VAR in --include arg' @("$N/dollar `$P22VAR.txt")
Test-Case 'literal %P22VAR% in --include arg' @("$N/percent %P22VAR% 100%.txt")
Test-Case 'literal ! and space' @("$N/bang !x!.txt", "$N/space in name.txt")
Test-Case 'literal {a,b} braces' @("$N/brace {a,b}.txt")
Test-Case 'non-ASCII name' @("$N/$uni.txt")
Test-Case 'case differs (C: vs c:) drive letter lower' @("/$($L.ToLower())/data/names/a.txt")
Test-Case 'case differs in file name A.TXT' @("$N/A.TXT")

# --include-file
Test-Case 'file: $P22VAR.txt (expanded -> p22value.txt?)' @() "$N/`$P22VAR.txt`n"
Test-Case 'file: dollar $P22VAR.txt' @() "$N/dollar `$P22VAR.txt`n"
Test-Case 'file: dollar $$P22VAR.txt' @() "$N/dollar `$`$P22VAR.txt`n"
Test-Case 'file: dollar ${P22VAR}' @() "$N/dollar `${P22VAR}.txt`n"
Test-Case 'file: dollar [$]P22VAR.txt' @() "$N/dollar [`$]P22VAR.txt`n"
Test-Case 'file: percent %P22VAR% 100%.txt' @() "$N/percent %P22VAR% 100%.txt`n"
Test-Case 'file: two names LF' @() "$N/a.txt`n$N/b.txt`n"
Test-Case 'file: two names CRLF' @() "$N/a.txt`n$N/b.txt`n" 'crlf'
Test-Case 'file: two names BOM' @() "$N/a.txt`n$N/b.txt`n" 'bom'
Test-Case 'file: two names BOM+CRLF' @() "$N/a.txt`n$N/b.txt`n" 'bom-crlf'
Test-Case 'file: non-ASCII name' @() "$N/$uni.txt`n"
Test-Case 'file: comment and blank line' @() "# comment`n`n$N/a.txt`n"
Test-Case 'file: trailing space' @() "$N/a.txt `n"
Test-Case 'file: class-escaped exact list' @() "$N/bracket [[]ab].txt`n$N/[[]x].txt`n$N/dollar [`$]P22VAR.txt`n$N/percent %P22VAR% 100%.txt`n$N/$uni.txt`n"
