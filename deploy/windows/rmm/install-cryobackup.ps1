# install-cryobackup.ps1 - install, upgrade or enroll the CryoBackup agent from an RMM.
#
# Runs as SYSTEM (or an elevated administrator). Idempotent: run it as often as
# you like; it installs when the agent is missing, upgrades when the package is
# newer, enrolls when a token is given and the device is not enrolled yet, and
# otherwise only reports.
#
# The enrollment token is never put on a command line, in an environment
# variable or in a file, and never printed: the MSI is installed through the
# Windows Installer API (MsiInstallProduct) from this process, not by starting
# msiexec.exe with TOKEN=... in its arguments, and a later enrollment writes it
# to the stdin of "cryo-agent.exe msi-configure", as the MSI itself does. (If
# the RMM puts the token into CRYO_ENROLL_TOKEN rather than a script variable,
# that environment variable is the RMM's doing; this script clears it.)
#
# Variables: set them as RMM script variables (the token as a secret / password
# variable); anything left unset falls back to the environment variable named.
#   $MsiPath      CRYO_MSI_PATH      path of cryobackup-agent.msi (required)
#   $MsiSha256    CRYO_MSI_SHA256    expected SHA-256 of that file (recommended)
#   $ServerUrl    CRYO_SERVER        console agent URL, https://host:port (with a token)
#   $EnrollToken  CRYO_ENROLL_TOKEN  enrollment token (secret; omit to install unenrolled)
#   $InsecureLab  CRYO_INSECURE_LAB  '1' allows a plain-http console, lab only
#   $LogPath      CRYO_MSI_LOG       verbose MSI log (default %windir%\Temp\cryobackup-install.log)
#
# Exit codes: 0 done (installed, upgraded, enrolled or already current),
# 1 not elevated or not 64-bit Windows, 2 bad input, 3 the MSI failed,
# 4 the result did not verify, 5 enrollment failed.

if (-not $MsiPath)     { $MsiPath     = $env:CRYO_MSI_PATH }
if (-not $MsiSha256)   { $MsiSha256   = $env:CRYO_MSI_SHA256 }
if (-not $ServerUrl)   { $ServerUrl   = $env:CRYO_SERVER }
if (-not $EnrollToken) { $EnrollToken = $env:CRYO_ENROLL_TOKEN }
$env:CRYO_ENROLL_TOKEN = $null   # read once; no child process inherits it
if (-not $InsecureLab) { $InsecureLab = $env:CRYO_INSECURE_LAB }
if (-not $LogPath)     { $LogPath     = $env:CRYO_MSI_LOG }
if (-not $LogPath)     { $LogPath     = Join-Path $env:windir 'Temp\cryobackup-install.log' }

$UpgradeCode = '{04B03054-481B-47AA-A586-4F275F1D5A51}'
$ServiceName = 'CryoBackup'
# The 64-bit Program Files also from a 32-bit RMM host (Program Files is not
# redirected for file access; only its name in $env:ProgramFiles differs).
$AgentExe    = Join-Path $env:ProgramW6432 'CryoBackup\cryo-agent.exe'

function Say($m) { Write-Output "cryobackup: $m" }

# Scrub: the token leaves this process's memory and environment on every exit.
function Done($code, $m) {
    if ($m) { Say $m }
    $script:EnrollToken = $null
    $env:CRYO_ENROLL_TOKEN = $null
    exit $code
}

# ---- discovery ---------------------------------------------------------------
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Done 1 'must run as SYSTEM or an elevated administrator' }
if (-not [Environment]::Is64BitOperatingSystem) { Done 1 'CryoBackup needs 64-bit Windows' }

if (-not $MsiPath -or -not (Test-Path -LiteralPath $MsiPath -PathType Leaf)) { Done 2 'MsiPath (CRYO_MSI_PATH) does not name the MSI' }
$MsiPath = (Resolve-Path -LiteralPath $MsiPath).Path
if ($MsiSha256) {
    $h = (Get-FileHash -LiteralPath $MsiPath -Algorithm SHA256).Hash
    if ($h -ne $MsiSha256.Trim().ToUpperInvariant()) { Done 2 "the MSI's SHA-256 is $h, expected ${MsiSha256}: refusing it" }
    Say 'MSI SHA-256 matches'
}
if ($EnrollToken -and -not $ServerUrl) { Done 2 'a token was given without ServerUrl (CRYO_SERVER)' }
if ($ServerUrl -and -not $EnrollToken) { Done 2 'ServerUrl was given without a token (CRYO_ENROLL_TOKEN)' }
if ($ServerUrl -and -not $ServerUrl.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase) -and $InsecureLab -ne '1') {
    Done 2 'ServerUrl must start with https:// (plain http only with CRYO_INSECURE_LAB=1, lab only)'
}
foreach ($v in @($ServerUrl, $EnrollToken)) {
    if ($v -and $v.Contains('"')) { Done 2 'ServerUrl and the token must not contain a double quote' }
}

$installer = New-Object -ComObject WindowsInstaller.Installer
function Invoke-Com($obj, $name, $kind, $argv) { return $obj.GetType().InvokeMember($name, $kind, $null, $obj, $argv) }
$db = Invoke-Com $installer 'OpenDatabase' 'InvokeMethod' @($MsiPath, 0)
$view = Invoke-Com $db 'OpenView' 'InvokeMethod' @("SELECT ``Value`` FROM ``Property`` WHERE ``Property`` = 'ProductVersion'")
Invoke-Com $view 'Execute' 'InvokeMethod' $null | Out-Null
$rec = Invoke-Com $view 'Fetch' 'InvokeMethod' $null
$pkgVersion = [version](Invoke-Com $rec 'StringData' 'GetProperty' @(1))
Invoke-Com $view 'Close' 'InvokeMethod' $null | Out-Null

$installed = $null
foreach ($pc in (Invoke-Com $installer 'RelatedProducts' 'GetProperty' @($UpgradeCode))) {
    $v = [version](Invoke-Com $installer 'ProductInfo' 'GetProperty' @($pc, 'VersionString'))
    if (-not $installed -or $v -gt $installed) { $installed = $v }
}
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue

function Test-Enrolled {
    if (-not (Test-Path -LiteralPath $AgentExe)) { return $false }
    $st = & $AgentExe status 2>$null
    return [bool]($st | Where-Object { $_ -match '^enrolled:\s+yes' })
}

Say "package $pkgVersion; installed $(if ($installed) { $installed } else { 'none' }); service $(if ($svc) { $svc.Status } else { 'absent' })"

# ---- act ---------------------------------------------------------------------
# All four version fields count. Windows Installer itself compares only X.Y.Z,
# but CryoBackup.wxs sets AllowSameVersionUpgrades, so a package with the same
# X.Y.Z replaces the installed one whatever its fourth field: an untagged
# build 0.0.0.217 does replace 0.0.0.214 (and 0.0.0.214 would replace 0.0.0.217,
# which is why this wrapper refuses it). Higher: upgrade. Lower: no downgrade.
# Equal: current. deploy/windows/ci/wrapper-version.ps1 tests this function.
function Get-CryoMsiAction($pkg, $inst) {
    if (-not $inst) { return 'install' }
    $p = [version]"$($pkg.Major).$($pkg.Minor).$([Math]::Max($pkg.Build, 0)).$([Math]::Max($pkg.Revision, 0))"
    $i = [version]"$($inst.Major).$($inst.Minor).$([Math]::Max($inst.Build, 0)).$([Math]::Max($inst.Revision, 0))"
    if ($p -gt $i) { return 'upgrade' }
    if ($p -lt $i) { return 'downgrade' }
    return 'current'
}
$action = Get-CryoMsiAction $pkgVersion $installed
$needMsi = $action -eq 'install' -or $action -eq 'upgrade'
if ($installed -and -not $needMsi -and -not $svc) {
    Done 4 "CryoBackup $installed is registered but its service is missing: uninstall it (msiexec /x {product code} or Apps and Features) and run this again; the device state in ProgramData is kept"
}
if ($action -eq 'downgrade') {
    Say "a newer version ($installed) is installed; this package ($pkgVersion) is not applied (no downgrade)"
}

if ($needMsi) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class CryoMsi {
    [DllImport("msi.dll", CharSet = CharSet.Unicode)] public static extern int MsiSetInternalUI(int level, IntPtr window);
    [DllImport("msi.dll", CharSet = CharSet.Unicode)] public static extern uint MsiEnableLog(uint mode, string file, uint attributes);
    [DllImport("msi.dll", CharSet = CharSet.Unicode)] public static extern uint MsiInstallProduct(string package, string properties);
}
'@
    $props = 'REBOOT=ReallySuppress'
    $enrolledBefore = Test-Enrolled
    if ($EnrollToken -and -not $enrolledBefore) {
        $props += ' SERVER="' + $ServerUrl + '" TOKEN="' + $EnrollToken + '"'
        if ($InsecureLab -eq '1') { $props += ' CRYO_INSECURE_LAB=1' }
    }
    [CryoMsi]::MsiSetInternalUI(2, [IntPtr]::Zero) | Out-Null          # INSTALLUILEVEL_NONE
    [CryoMsi]::MsiEnableLog(0x1FFF, $LogPath, 0) | Out-Null            # verbose; TOKEN is a hidden property
    Say "running the MSI (log: $LogPath)"
    $rc = [CryoMsi]::MsiInstallProduct($MsiPath, $props)
    $props = $null
    if ($rc -eq 3010) { Say 'the MSI asks for a reboot (suppressed)' }
    elseif ($rc -ne 0) { Done 3 "the MSI failed with $rc; the reason is in $LogPath (search for 'CryoBackup:' and 'Return value 3')" }
    Say 'MSI applied'
} elseif ($EnrollToken -and -not (Test-Enrolled)) {
    # Installed and current but not enrolled: enroll now, the way the MSI does
    # (cryo-agent msi-configure, the request and the token on its stdin only;
    # it prepares the state directory and the service as "install" does).
    $rng = New-Object Security.Cryptography.RNGCryptoServiceProvider
    $nb = New-Object byte[] 16; $rng.GetBytes($nb)
    $nonce = -join ($nb | ForEach-Object { $_.ToString('x2') })
    $lab = ''; if ($InsecureLab -eq '1') { $lab = '1' }
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = $AgentExe
    $psi.Arguments = 'msi-configure'
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [Diagnostics.Process]::Start($psi)
    $p.StandardInput.Write("server=$ServerUrl`ntoken=$EnrollToken`nlab=$lab`nrepair=`nnonce=$nonce`n")
    $p.StandardInput.Close()
    $errTask = $p.StandardError.ReadToEndAsync()
    $out = $p.StandardOutput.ReadToEnd()
    $p.WaitForExit()
    $code = $p.ExitCode
    "$out`n$($errTask.Result)".Split("`n") | Where-Object { $_.Trim() } | ForEach-Object { Say "agent: $($_.TrimEnd())" }
    & $AgentExe msi-commit | Out-Null
    if ($code -ne 0) { Done 5 "enrollment failed (cryo-agent exit $code)" }
    Start-Service -Name $ServiceName -ErrorAction SilentlyContinue
} else {
    Say 'already installed and current; nothing to do'
}

# ---- verify ------------------------------------------------------------------
$svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $svc) { Done 4 "the $ServiceName service is missing after the run" }
if (-not (Test-Path -LiteralPath $AgentExe)) { Done 4 "$AgentExe is missing after the run" }
$enrolled = Test-Enrolled
if ($enrolled) {
    if ($svc.Status -ne 'Running') { Start-Service -Name $ServiceName -ErrorAction SilentlyContinue }
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline -and (Get-Service -Name $ServiceName).Status -ne 'Running') { Start-Sleep -Seconds 2 }
    if ((Get-Service -Name $ServiceName).Status -ne 'Running') { Done 4 "the device is enrolled but the $ServiceName service is not running (see the Application Event Log, source CryoBackup)" }
}

# ---- report ------------------------------------------------------------------
if ($enrolled) {
    Done 0 "CryoBackup is installed, enrolled and running"
}
Done 0 "CryoBackup is installed but NOT ENROLLED: run this again with ServerUrl and EnrollToken, or run 'cryo-agent enroll' elevated"
