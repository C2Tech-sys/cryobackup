# wrapper-version.ps1 - unit check of the RMM wrapper's version rule (L-UPG-1).
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\ci\wrapper-version.ps1
#
# Loads only the Get-CryoMsiAction function from rmm\install-cryobackup.ps1
# (parsed, not run: the wrapper itself needs elevation and an MSI) and checks
# that all four version fields decide: a higher package upgrades, a lower one
# is a downgrade (refused), an equal one is current. Needs no elevation and no
# test framework. smoke.ps1 runs it first, so the CI MSI job runs it.
#
# Exit codes: 0 every case passed, 2 the function was not found, 6 a case failed.

$Wrapper = $env:CRYO_WRAPPER   # optional: the wrapper to check; default ..\rmm\install-cryobackup.ps1
if (-not $Wrapper) { $Wrapper = Join-Path $PSScriptRoot '..\rmm\install-cryobackup.ps1' }

function Say($m) { Write-Host "wrapper-version: $m" }

# ---- discovery ---------------------------------------------------------------
$tokens = $null; $errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path -LiteralPath $Wrapper).Path, [ref]$tokens, [ref]$errors)
if ($errors.Count -gt 0) { Say "$Wrapper does not parse: $($errors[0].Message)"; exit 2 }
$fn = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Get-CryoMsiAction' }, $true)
if (-not $fn) { Say "Get-CryoMsiAction is not defined in $Wrapper"; exit 2 }
. ([scriptblock]::Create($fn.Extent.Text))

# ---- act / verify ------------------------------------------------------------
# package, installed (empty = none), expected action
$cases = @(
    @('0.0.0.217', '',          'install'),
    @('0.0.0.217', '0.0.0.214', 'upgrade'),     # the lab case: same X.Y.Z, higher fourth field
    @('0.0.0.214', '0.0.0.217', 'downgrade'),   # the MSI alone would apply it (AllowSameVersionUpgrades)
    @('0.0.0.217', '0.0.0.217', 'current'),
    @('0.0.1',     '0.0.1',     'current'),
    @('0.0.1.0',   '0.0.1',     'current'),     # a missing fourth field is 0
    @('0.0.1.3',   '0.0.1',     'upgrade'),
    @('0.0.1',     '0.0.1.3',   'downgrade'),
    @('0.0.2',     '0.0.1.40',  'upgrade'),     # X.Y.Z still outranks the fourth field
    @('0.0.1.40',  '0.0.2',     'downgrade'),
    @('1.0.0.0',   '0.9.9.999', 'upgrade')
)
$failed = 0
foreach ($c in $cases) {
    $inst = $null
    if ($c[1]) { $inst = [version]$c[1] }
    $got = Get-CryoMsiAction ([version]$c[0]) $inst
    $label = "package $($c[0]), installed $(if ($c[1]) { $c[1] } else { 'none' })"
    if ($got -eq $c[2]) { Say "ok: $label -> $got" } else { Say "FAIL: $label -> $got, want $($c[2])"; $failed++ }
}

# ---- report ------------------------------------------------------------------
if ($failed -gt 0) { Say "$failed case(s) failed"; exit 6 }
Say "all $($cases.Count) cases passed"
exit 0
