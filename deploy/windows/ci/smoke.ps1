# smoke.ps1 - install, upgrade and uninstall the agent MSI for real (CI).
#
# Needs an elevated, disposable Windows machine with no CryoBackup on it (the
# windows-latest runner). It installs services, writes HKLM and ProgramData,
# and leaves ProgramData\CryoBackup behind on purpose (that is one of the
# things it checks). Do not run it on a machine you care about.
#
#   $env:CRYO_SMOKE_MSI1        an MSI built as version 0.0.1
#   $env:CRYO_SMOKE_MSI2        an MSI built as version 0.0.2
#   $env:CRYO_SMOKE_CONSOLE     fakeconsole.exe (deploy/windows/ci/fakeconsole)
#   $env:CRYO_SMOKE_LOGS        folder for the MSI logs (uploaded by CI)
#
# Exit codes: 0 every check passed, 1 not elevated or the machine is not clean,
# 2 bad input, 6 a check failed.

$Msi1    = $env:CRYO_SMOKE_MSI1
$Msi2    = $env:CRYO_SMOKE_MSI2
$Console = $env:CRYO_SMOKE_CONSOLE
$Logs    = $env:CRYO_SMOKE_LOGS

$ServiceName = 'CryoBackup'
$UpgradeCode = '{04B03054-481B-47AA-A586-4F275F1D5A51}'
$StateDir    = Join-Path $env:ProgramData 'CryoBackup'
$InstallDir  = Join-Path $env:ProgramFiles 'CryoBackup'
$AgentExe    = Join-Path $InstallDir 'cryo-agent.exe'
$EventKey    = 'HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\CryoBackup'
$ConsolePort = 18080
$Wrapper     = Join-Path $PSScriptRoot '..\rmm\install-cryobackup.ps1'
$Started     = Get-Date

. (Join-Path $PSScriptRoot '..\pin.ps1')
$script:failed = 0
function Say($m)  { Write-Host "smoke: $m" }
function Bad($m)  { Write-Host "smoke: FAIL: $m"; $script:failed++ }
function Check($ok, $m) { if ($ok) { Write-Host "smoke: ok: $m" } else { Bad $m } }

# Two tokens that must never be found anywhere after the run.
$BadToken  = 'smoke-bad-' + [guid]::NewGuid().ToString('N')
$GoodToken = 'smoke-good-' + [guid]::NewGuid().ToString('N')

function Invoke-Msi($name, $msiArgs) {
    $log = Join-Path $Logs "$name.log"
    $p = Start-Process -FilePath msiexec.exe -ArgumentList "$msiArgs /qn /norestart /l*v `"$log`"" -Wait -PassThru
    Say "$name -> msiexec exit $($p.ExitCode)"
    return @{ Code = $p.ExitCode; Log = $log }
}
function LogHas($r, $text) { return [bool](Select-String -LiteralPath $r.Log -SimpleMatch $text -Quiet) }
function Svc { return Get-Service -Name $ServiceName -ErrorAction SilentlyContinue }
function Installed {
    $inst = New-Object -ComObject WindowsInstaller.Installer
    $out = @()
    foreach ($pc in $inst.GetType().InvokeMember('RelatedProducts', 'GetProperty', $null, $inst, @($UpgradeCode))) {
        $out += $inst.GetType().InvokeMember('ProductInfo', 'GetProperty', $null, $inst, @($pc, 'VersionString'))
    }
    return , $out
}
function Status { if (Test-Path -LiteralPath $AgentExe) { return (& $AgentExe status 2>&1) -join "`n" } return '' }
function WaitRunning($seconds) {
    $deadline = (Get-Date).AddSeconds($seconds)
    while ((Get-Date) -lt $deadline) {
        $s = Svc
        if ($s -and $s.Status -eq 'Running') { return $true }
        Start-Sleep -Seconds 2
    }
    return $false
}
function CheckServiceConfig($label) {
    $qc = (& sc.exe qc $ServiceName) -join "`n"
    Check ($qc -match 'AUTO_START\s+\(DELAYED\)') "$label - service is automatic (delayed start)"
    Check ($qc -match 'LocalSystem') "$label - service runs as LocalSystem"
    Check ($qc -match [regex]::Escape('"' + $AgentExe + '" run --state-dir')) "$label - service command line runs cryo-agent.exe run --state-dir"
    $qf = (& sc.exe qfailure $ServiceName) -join "`n"
    Check ($qf -match 'RESET_PERIOD \(in seconds\)\s*:\s*86400') "$label - failure counter resets after a day"
    Check ($qf -match 'RESTART -- Delay = 60000' -and $qf -match 'RESTART -- Delay = 300000' -and $qf -match 'RESTART -- Delay = 900000') "$label - restart after 1, 5 and 15 minutes"
    $pre = (Get-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName" -Name PreshutdownTimeout -ErrorAction SilentlyContinue).PreshutdownTimeout
    Check ($pre -eq 120000) "$label - pre-shutdown timeout 120000 ms (is $pre)"
    Check (Test-Path -LiteralPath $EventKey) "$label - Event Log source registered"
    # L-EVT-1: the message file is the installed cryo-agent.exe (its message
    # table has every agent id), not EventCreate.exe (ids 1 to 1000 only).
    $mf = (Get-ItemProperty -LiteralPath $EventKey -Name EventMessageFile -ErrorAction SilentlyContinue).EventMessageFile
    Check ($mf -and ([Environment]::ExpandEnvironmentVariables($mf) -eq $AgentExe)) "$label - Event Log message file is $AgentExe (is $mf)"
}
function CheckRemoved($label) {
    Check (-not (Svc)) "$label - service removed"
    Check (-not (Test-Path -LiteralPath $InstallDir)) "$label - $InstallDir removed"
    Check (-not (Test-Path -LiteralPath $EventKey)) "$label - Event Log source removed"
    Check ((Installed).Count -eq 0) "$label - no CryoBackup product registered"
}

# ---- discovery ---------------------------------------------------------------
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { Say 'must run elevated'; exit 1 }
foreach ($f in @($Msi1, $Msi2, $Console)) { if (-not $f -or -not (Test-Path -LiteralPath $f)) { Say "missing input: $f"; exit 2 } }
if (-not $Logs) { Say 'CRYO_SMOKE_LOGS is not set'; exit 2 }
New-Item -ItemType Directory -Force -Path $Logs | Out-Null
if ((Svc) -or (Test-Path -LiteralPath $StateDir) -or (Installed).Count -gt 0) { Say 'this machine already has CryoBackup on it; the smoke test needs a clean one'; exit 1 }
$pin = Read-ResticPin (Join-Path $PSScriptRoot '..\restic.pin')

# ---- 0: the RMM wrapper's version rule (L-UPG-1) --------------------------------
# CI builds 0.0.1 and 0.0.2, which differ in the third field, so the fourth-
# field cases (an untagged 0.0.0.N over 0.0.0.M) are a unit check of the
# wrapper's Get-CryoMsiAction; steps 6 and 14 run the wrapper for real.
& powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'wrapper-version.ps1') | ForEach-Object { Say $_ }
Check ($LASTEXITCODE -eq 0) 'the RMM wrapper applies a higher four-part version, refuses a lower one and treats an equal one as current (wrapper-version.ps1)'

# ---- 1: enrollment failure on a clean machine rolls everything back -----------
$r = Invoke-Msi '01-enroll-fails' "/i `"$Msi1`" TOKEN=$BadToken SERVER=https://127.0.0.1:9"
Check ($r.Code -eq 1603) "enroll failure fails the install (1603, got $($r.Code))"
Check (LogHas $r 'error class: network') 'the log names the error class'
Check (LogHas $r 'CryoBackup: cryo-agent msi-configure failed') 'the log says the configure action failed'
Check (-not (Svc)) 'no service after the rollback'
Check (-not (Test-Path -LiteralPath $StateDir)) 'no state directory after the rollback (it was created by this install)'
Check ((Installed).Count -eq 0) 'no product registered after the rollback'

# ---- 2: plain http is refused before anything changes --------------------------
$r = Invoke-Msi '02-http-refused' "/i `"$Msi1`" TOKEN=$BadToken SERVER=http://127.0.0.1:9"
Check ($r.Code -eq 1603) "http SERVER without the lab property is refused (1603, got $($r.Code))"
Check (LogHas $r 'SERVER must start with https://') 'the log says why'
Check (-not (Svc)) 'no service'

# ---- 3: install without TOKEN: installed, unenrolled, said so -------------------
$r = Invoke-Msi '03-install-unenrolled' "/i `"$Msi1`""
Check ($r.Code -eq 0) "install without TOKEN succeeds (got $($r.Code))"
Check (LogHas $r 'NOT ENROLLED') 'the log says the device is not enrolled'
Check ([bool](Svc)) 'service exists after install'
CheckServiceConfig 'install'
Check (Test-Path -LiteralPath $StateDir) 'state directory created'
Check (-not (Test-Path -LiteralPath (Join-Path $StateDir '.msi-install-created'))) 'install marker removed by the commit action'
Check ((Get-Sha256 (Join-Path $InstallDir 'restic.exe')) -eq $pin.exe_sha256) 'installed restic.exe matches the pin'
$sentinel = Join-Path $StateDir 'smoke-sentinel.txt'
Set-Content -LiteralPath $sentinel -Value 'kept across upgrade and uninstall'

# ---- 4: the same install again is a no-op -------------------------------------
$r = Invoke-Msi '04-install-again' "/i `"$Msi1`""
Check ($r.Code -eq 0) "installing the same package again succeeds (got $($r.Code))"
Check ([bool](Svc)) 'service still exists'

# ---- 4b: a failed upgrade rolls back to the installed version -------------------
# The enrollment fails after the old version was removed inside the transaction.
$r = Invoke-Msi '04b-upgrade-fails' "/i `"$Msi2`" TOKEN=$BadToken SERVER=https://127.0.0.1:9"
Check ($r.Code -eq 1603) "an upgrade whose enrollment fails is rolled back (1603, got $($r.Code))"
$v = Installed
Check ($v.Count -eq 1 -and $v[0] -eq '0.0.1') "0.0.1 is still the installed version (have: $($v -join ', '))"
Check ([bool](Svc)) 'the service is back after the rolled-back upgrade'
Check (Test-Path -LiteralPath $AgentExe) 'cryo-agent.exe is back after the rolled-back upgrade'
Check (Test-Path -LiteralPath $sentinel) 'state that existed before the upgrade is kept by its rollback'
# Windows Installer restores the service from its own record, which has no
# delayed start, recovery actions or pre-shutdown budget: the drift is real...
$qc = (& sc.exe qc $ServiceName) -join "`n"
Check (-not ($qc -match 'AUTO_START\s+\(DELAYED\)')) "the rolled-back service has lost delayed start (the drift the agent must repair is real)"
# ...and the service's next start repairs it (EnsureConfig), loudly: Event Log 1009.
$beforeStart = Get-Date
& sc.exe start $ServiceName | Out-Null
Start-Sleep -Seconds 8
CheckServiceConfig 'after the rolled-back upgrade and one service start'
$ev = Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'CryoBackup'; Id = 1009; StartTime = $beforeStart } -ErrorAction SilentlyContinue
# L-EVT-1: .Message is what Event Viewer shows. It is rendered from the
# source's message file (cryo-agent.exe), so it is the agent's line; with
# EventCreate.exe it was empty for every id above 1000.
$evText = ''
if ($ev) { $evText = "$(($ev | Select-Object -First 1).Message)" }
Check ($evText -match 'had lost') "Event Log 1009 renders its text, as in Event Viewer, and says the service configuration was repaired ($(@($ev).Count) event(s): $evText)"
Check ([bool](Select-String -LiteralPath (Join-Path $StateDir 'logs\agent.log') -SimpleMatch 'had lost' -Quiet -ErrorAction SilentlyContinue)) 'agent.log says the service configuration was repaired'

# ---- 5: uninstall keeps the state ---------------------------------------------
$r = Invoke-Msi '05-uninstall' "/x `"$Msi1`""
Check ($r.Code -eq 0) "uninstall succeeds (got $($r.Code))"
CheckRemoved 'uninstall'
Check (Test-Path -LiteralPath $sentinel) 'ProgramData state kept by uninstall'

# ---- 6: install with TOKEN through the RMM wrapper: enrolled and running ---------
$env:CRYO_FAKE_TOKEN = $GoodToken
$fake = Start-Process -FilePath $Console -ArgumentList "-listen 127.0.0.1:$ConsolePort" -PassThru -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $Logs 'fakeconsole.out') -RedirectStandardError (Join-Path $Logs 'fakeconsole.err')
$env:CRYO_FAKE_TOKEN = $null
Start-Sleep -Seconds 2
try {
    $env:CRYO_MSI_PATH = $Msi1
    $env:CRYO_SERVER = "http://127.0.0.1:$ConsolePort"
    $env:CRYO_ENROLL_TOKEN = $GoodToken
    $env:CRYO_INSECURE_LAB = '1'
    $env:CRYO_MSI_LOG = Join-Path $Logs '06-wrapper-enroll.log'
    $out = & powershell -NoProfile -ExecutionPolicy Bypass -File $Wrapper 2>&1
    $code = $LASTEXITCODE
    $env:CRYO_ENROLL_TOKEN = $null; $env:CRYO_SERVER = $null; $env:CRYO_INSECURE_LAB = $null
    $out | ForEach-Object { Say "wrapper: $_" }
    Set-Content -LiteralPath (Join-Path $Logs '06-wrapper.out') -Value $out
    Check ($code -eq 0) "RMM wrapper installs and enrolls (exit $code)"
    Check (WaitRunning 60) 'service running after the enrolled install'
    $st = Status
    Check ($st -match 'enrolled:\s+yes' -and $st.Contains('01JB0000000000000000000M51')) 'cryo-agent status shows the enrolled device'
    CheckServiceConfig 'enrolled install'

    # The wrapper again: nothing to do.
    $env:CRYO_MSI_LOG = Join-Path $Logs '06b-wrapper-again.log'
    $out = & powershell -NoProfile -ExecutionPolicy Bypass -File $Wrapper 2>&1
    $code = $LASTEXITCODE
    $out | ForEach-Object { Say "wrapper: $_" }
    Check ($code -eq 0 -and ($out -join ' ') -match 'nothing to do') "RMM wrapper is idempotent (exit $code)"
    $env:CRYO_MSI_PATH = $null; $env:CRYO_MSI_LOG = $null

    # ---- 7: upgrade in place: service restarted, identity and state kept -----------
    $r = Invoke-Msi '07-upgrade' "/i `"$Msi2`""
    Check ($r.Code -eq 0) "upgrade to 0.0.2 succeeds (got $($r.Code))"
    $v = Installed
    Check ($v.Count -eq 1 -and $v[0] -eq '0.0.2') "exactly one product registered, version 0.0.2 (have: $($v -join ', '))"
    Check (LogHas $r 'enrolled: device 01JB0000000000000000000M51') 'the upgrade log names the kept enrollment'
    Check (WaitRunning 60) 'service running after the upgrade'
    Check ((Status).Contains('01JB0000000000000000000M51')) 'device identity kept by the upgrade'
    Check (Test-Path -LiteralPath $sentinel) 'state kept by the upgrade'

    # ---- 8: downgrade refused ------------------------------------------------------
    $r = Invoke-Msi '08-downgrade' "/i `"$Msi1`""
    Check ($r.Code -ne 0) "downgrade to 0.0.1 is refused (got $($r.Code))"
    Check (LogHas $r 'Downgrading is refused') 'the log says why'
    $v = Installed
    Check ($v.Count -eq 1 -and $v[0] -eq '0.0.2') "0.0.2 still installed (have: $($v -join ', '))"
    Check (WaitRunning 30) 'service still running'

    # ---- 9: uninstall a running, enrolled agent ------------------------------------
    $r = Invoke-Msi '09-uninstall' "/x `"$Msi2`""
    Check ($r.Code -eq 0) "uninstall succeeds (got $($r.Code))"
    CheckRemoved 'uninstall of the enrolled agent'
    Check (Test-Path -LiteralPath (Join-Path $StateDir 'config\agent.json')) 'agent.json kept by uninstall'

    # ---- 10: reinstall: the kept identity is picked up --------------------------------
    $r = Invoke-Msi '10-reinstall' "/i `"$Msi2`""
    Check ($r.Code -eq 0) "reinstall succeeds (got $($r.Code))"
    Check (WaitRunning 60) 'service running after the reinstall'
    Check ((Status).Contains('01JB0000000000000000000M51')) 'reinstall uses the kept identity'

    # ---- 11: uninstall again -----------------------------------------------------------
    $r = Invoke-Msi '11-uninstall' "/x `"$Msi2`""
    Check ($r.Code -eq 0) "second uninstall succeeds (got $($r.Code))"
    CheckRemoved 'second uninstall'
    Check (Test-Path -LiteralPath $sentinel) 'state kept by the second uninstall'

    # ---- 12: a clean machine, TOKEN and SERVER on msiexec: enrolled in one step -----------
    Remove-Item -LiteralPath $StateDir -Recurse -Force
    Check (-not (Test-Path -LiteralPath $StateDir)) 'state directory removed for a clean-machine enrolled install'
    $r = Invoke-Msi '12-install-enrolled' "/i `"$Msi2`" TOKEN=$GoodToken SERVER=http://127.0.0.1:$ConsolePort CRYO_INSECURE_LAB=1"
    Check ($r.Code -eq 0) "silent install with TOKEN and SERVER succeeds (got $($r.Code))"
    Check (LogHas $r 'enrolled: device 01JB0000000000000000000M51') 'the log names the new enrollment'
    Check (WaitRunning 60) 'service running after the one-step enrolled install'
    Check (-not (Test-Path -LiteralPath (Join-Path $StateDir '.msi-install-created'))) 'install marker removed by the commit action'
    CheckServiceConfig 'one-step enrolled install'

    # ---- 13: final uninstall -------------------------------------------------------------
    $r = Invoke-Msi '13-uninstall' "/x `"$Msi2`""
    Check ($r.Code -eq 0) "final uninstall succeeds (got $($r.Code))"
    CheckRemoved 'final uninstall'
    Check (Test-Path -LiteralPath (Join-Path $StateDir 'config\agent.json')) 'agent.json kept by the final uninstall'

    # ---- 14: installed but unenrolled, then the RMM wrapper enrolls it (stdin path) -----
    Remove-Item -LiteralPath $StateDir -Recurse -Force
    $r = Invoke-Msi '14-install-unenrolled' "/i `"$Msi2`""
    Check ($r.Code -eq 0) "unenrolled install for the enroll-later path (got $($r.Code))"
    $env:CRYO_MSI_PATH = $Msi2
    $env:CRYO_SERVER = "http://127.0.0.1:$ConsolePort"
    $env:CRYO_ENROLL_TOKEN = $GoodToken
    $env:CRYO_INSECURE_LAB = '1'
    $env:CRYO_MSI_LOG = Join-Path $Logs '14-wrapper-enroll-later.log'
    $out = & powershell -NoProfile -ExecutionPolicy Bypass -File $Wrapper 2>&1
    $code = $LASTEXITCODE
    $env:CRYO_ENROLL_TOKEN = $null; $env:CRYO_SERVER = $null; $env:CRYO_INSECURE_LAB = $null; $env:CRYO_MSI_PATH = $null; $env:CRYO_MSI_LOG = $null
    $out | ForEach-Object { Say "wrapper: $_" }
    Set-Content -LiteralPath (Join-Path $Logs '14-wrapper.out') -Value $out
    Check ($code -eq 0 -and ($out -join ' ') -match 'agent: enrolled: device 01JB0000000000000000000M51') "RMM wrapper enrolls an installed device through msi-configure (exit $code)"
    # "enrolled: device" is also what the old CRYO_ENROLL_TOKEN path printed;
    # these two lines are msi-configure's own (cmd/cryo-agent/msi.go), so a
    # wrapper that stops using it fails here.
    Check (($out -join ' ') -match 'agent: service CryoBackup configured: LocalSystem, delayed automatic start') 'the wrapper ran msi-configure: it configured the service'
    Check (($out -join ' ') -match 'agent: state directory exists: configuration, identity and journal are kept') 'the wrapper ran msi-configure: it kept the installed state directory'
    Check (-not (Test-Path -LiteralPath (Join-Path $Logs '14-wrapper-enroll-later.log'))) 'the enroll-later path ran no MSI'
    Check (WaitRunning 60) 'service running after the enroll-later path'
    $r = Invoke-Msi '14b-uninstall' "/x `"$Msi2`""
    Check ($r.Code -eq 0) "uninstall after the enroll-later path (got $($r.Code))"
    CheckRemoved 'uninstall after the enroll-later path'

    # ---- 15: CRYO_REPAIR_ACL=1 on an untrusted state directory, and the install fails -----
    Remove-Item -LiteralPath $StateDir -Recurse -Force
    New-Item -ItemType Directory -Path $StateDir | Out-Null
    & icacls.exe $StateDir /grant '*S-1-1-0:(OI)(CI)F' | Out-Null       # Everyone: full control (an opened ACL)
    Set-Content -LiteralPath (Join-Path $StateDir 'planted.txt') -Value 'untrusted content'
    $r = Invoke-Msi '15a-untrusted-refused' "/i `"$Msi2`""
    Check ($r.Code -eq 1603) "an untrusted state directory refuses the install (1603, got $($r.Code))"
    Check (LogHas $r 'refusing the state directory') 'the log says the state directory is refused'
    Check (-not (Svc)) 'no service after the refused install'
    Check (Test-Path -LiteralPath (Join-Path $StateDir 'planted.txt')) 'the refused directory is left as it was'
    $r = Invoke-Msi '15b-repair-fails' "/i `"$Msi2`" CRYO_REPAIR_ACL=1 TOKEN=$BadToken SERVER=https://127.0.0.1:9"
    Check ($r.Code -eq 1603) "a repair install whose enrollment fails is rolled back (1603, got $($r.Code))"
    Check (LogHas $r 'it stays there (the rollback does not move it back') 'the log says the quarantine stays and how to recover'
    $q = @(Get-ChildItem -LiteralPath $env:ProgramData -Directory -Filter 'CryoBackup.quarantine-*')
    Check ($q.Count -eq 1 -and (Test-Path -LiteralPath (Join-Path $q[0].FullName 'planted.txt'))) "the untrusted tree is in its quarantine ($($q.FullName -join ', '))"
    Check (-not (Test-Path -LiteralPath $StateDir)) 'the fresh state directory the repair run created is removed by the failure'
    Check (-not (Svc)) 'no service after the failed repair install'
    Check ((Installed).Count -eq 0) 'no product registered after the failed repair install'
} finally {
    if ($fake -and -not $fake.HasExited) { Stop-Process -Id $fake.Id -Force }
}

# ---- 14: neither token survives anywhere it could have been written ------------------
function Find-Token($token, $files) {
    $a = [Text.Encoding]::ASCII.GetBytes($token)
    $u = [Text.Encoding]::Unicode.GetBytes($token)
    $hits = @()
    foreach ($f in $files) {
        try { $b = [IO.File]::ReadAllBytes($f.FullName) } catch { continue }
        $s1 = [Text.Encoding]::GetEncoding(28591).GetString($b)
        if ($s1.Contains([Text.Encoding]::GetEncoding(28591).GetString($a)) -or $s1.Contains([Text.Encoding]::GetEncoding(28591).GetString($u))) { $hits += $f.FullName }
    }
    return , $hits
}
$regDump = Join-Path $Logs 'registry-installer.reg.txt'
Remove-Item -LiteralPath $regDump -ErrorAction SilentlyContinue
foreach ($k in @('HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Installer', 'HKLM\SOFTWARE\Classes\Installer', 'HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall', 'HKLM\SYSTEM\CurrentControlSet\Services\CryoBackup', 'HKLM\SOFTWARE\CryoBackup')) {
    $tmpReg = Join-Path $env:TEMP ('smoke-' + [guid]::NewGuid().ToString('N') + '.reg')
    & reg.exe export $k $tmpReg /y 2>$null | Out-Null
    if (Test-Path -LiteralPath $tmpReg) { Get-Content -LiteralPath $tmpReg | Add-Content -LiteralPath $regDump; Remove-Item -LiteralPath $tmpReg }
}
# The scanner itself must find a token where one is (UTF-16 and ASCII).
$canary = Join-Path $env:TEMP ('smoke-canary-' + [guid]::NewGuid().ToString('N'))
[IO.File]::WriteAllText("$canary.u16", "x $GoodToken x", [Text.Encoding]::Unicode)
[IO.File]::WriteAllText("$canary.a", "x $BadToken x", [Text.Encoding]::ASCII)
Check ((Find-Token $GoodToken @(Get-Item "$canary.u16")).Count -eq 1) 'the scanner finds a UTF-16 token'
Check ((Find-Token $BadToken @(Get-Item "$canary.a")).Count -eq 1) 'the scanner finds an ASCII token'
Remove-Item -LiteralPath "$canary.u16", "$canary.a"

$scan = @()
$scan += Get-ChildItem -LiteralPath $Logs -Recurse -File -ErrorAction SilentlyContinue
$scan += Get-ChildItem -LiteralPath $StateDir -Recurse -File -Force -ErrorAction SilentlyContinue
foreach ($d in @((Join-Path $env:windir 'Installer'), (Join-Path $env:windir 'Temp'), $env:TEMP)) {
    $scan += Get-ChildItem -LiteralPath $d -Recurse -File -Force -ErrorAction SilentlyContinue |
        Where-Object { ($_.LastWriteTime -ge $Started -or $_.CreationTime -ge $Started) -and $_.Length -lt 200MB }
}
Say "scanning $($scan.Count) files written during the run for the tokens"
foreach ($t in @($BadToken, $GoodToken)) {
    $hits = Find-Token $t $scan
    Check ($hits.Count -eq 0) "token not found in any log, state, installer cache, temp file or registry export ($($hits -join ', '))"
}
Check ((Select-String -LiteralPath (Join-Path $Logs '01-enroll-fails.log') -SimpleMatch 'TOKEN' -Quiet)) 'the failing log mentions the TOKEN property (masked), so the scan looked at the right file'

# ---- report ----------------------------------------------------------------------------
if ($script:failed -gt 0) { Say "$($script:failed) check(s) failed"; exit 6 }
Say 'all checks passed'
exit 0
