# verify.ps1 - check a built CryoBackup agent MSI without installing it.
#
#   $env:CRYO_VERIFY_MSI = 'dist\windows\cryobackup-agent.msi'
#   $env:CRYO_VERIFY_SIGNED = '1'     # a signed release build; '0' or unset for an unsigned build
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\verify.ps1
#
# Runs after every build (build.ps1) and in CI. Needs no elevation: the files
# are taken out with an administrative extract (msiexec /a), nothing is
# installed. Checks:
#   - the package tables: fixed UpgradeCode, TOKEN and the configure action's
#     data hidden from the log, the configure action marked HideTarget;
#   - restic.exe in the package is byte-identical to the pinned upstream file;
#   - cryo-agent.exe in the package carries the same pin and runs;
#   - signatures: with CRYO_VERIFY_SIGNED=1 cryo-agent.exe and the MSI must be
#     Valid; restic.exe is never re-signed, so its signature is whatever
#     upstream shipped (it only has to match the pin).
# Exit codes: 0 verified, 2 bad input, 6 a check failed.

$Msi      = $env:CRYO_VERIFY_MSI
$WantSign = ($env:CRYO_VERIFY_SIGNED -eq '1')

$UpgradeCode = '{04B03054-481B-47AA-A586-4F275F1D5A51}'
. (Join-Path $PSScriptRoot 'pin.ps1')

$script:failed = 0
function Say($m)  { Write-Host "verify: $m" }
function Bad($m)  { Write-Host "verify: FAIL: $m"; $script:failed++ }
function Good($m) { Write-Host "verify: ok: $m" }

# ---- discovery ---------------------------------------------------------------
$pin = Read-ResticPin (Join-Path $PSScriptRoot 'restic.pin')
if (-not $pin) { Say 'the restic pin file is missing or incomplete'; exit 2 }
if (-not $Msi -or -not (Test-Path -LiteralPath $Msi -PathType Leaf)) { Say 'CRYO_VERIFY_MSI does not name an MSI'; exit 2 }
$Msi = (Resolve-Path -LiteralPath $Msi).Path

# ---- package tables ----------------------------------------------------------
function Get-MsiRows($path, $sql) {
    $inst = New-Object -ComObject WindowsInstaller.Installer
    $db = $inst.GetType().InvokeMember('OpenDatabase', 'InvokeMethod', $null, $inst, @($path, 0))
    $view = $db.GetType().InvokeMember('OpenView', 'InvokeMethod', $null, $db, @($sql))
    $view.GetType().InvokeMember('Execute', 'InvokeMethod', $null, $view, $null) | Out-Null
    $rows = @()
    while ($true) {
        $rec = $view.GetType().InvokeMember('Fetch', 'InvokeMethod', $null, $view, $null)
        if ($null -eq $rec) { break }
        $n = $rec.GetType().InvokeMember('FieldCount', 'GetProperty', $null, $rec, $null)
        $row = @()
        for ($i = 1; $i -le $n; $i++) { $row += $rec.GetType().InvokeMember('StringData', 'GetProperty', $null, $rec, @($i)) }
        $rows += , $row
    }
    $view.GetType().InvokeMember('Close', 'InvokeMethod', $null, $view, $null) | Out-Null
    return , $rows
}
function Get-MsiProperty($path, $name) {
    $r = Get-MsiRows $path "SELECT ``Value`` FROM ``Property`` WHERE ``Property`` = '$name'"
    if ($r.Count -eq 0) { return $null }
    return $r[0][0]
}

$uc = Get-MsiProperty $Msi 'UpgradeCode'
if ($uc -eq $UpgradeCode) { Good "UpgradeCode $uc" } else { Bad "UpgradeCode is '$uc', must be $UpgradeCode" }
$pv = Get-MsiProperty $Msi 'ProductVersion'
Say "ProductVersion $pv"
$hidden = "$(Get-MsiProperty $Msi 'MsiHiddenProperties')".Split(';')
foreach ($p in @('TOKEN', 'CryoConfigure')) {
    if ($hidden -contains $p) { Good "$p is in MsiHiddenProperties" } else { Bad "$p is not in MsiHiddenProperties (its value would be logged)" }
}
$ca = Get-MsiRows $Msi "SELECT ``Type`` FROM ``CustomAction`` WHERE ``Action`` = 'CryoConfigure'"
if ($ca.Count -eq 1 -and (([int]$ca[0][0]) -band 0x2000)) { Good 'CryoConfigure is HideTarget' } else { Bad 'CryoConfigure is missing or not HideTarget (its data would be logged)' }

# The custom-action DLL lives in the Binary table, not among the files an
# administrative extract writes: read its stream out with the MSI API.
function Save-MsiBinary($path, $name, $out) {
    if (-not ('CryoMsiDb' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.IO;
using System.Runtime.InteropServices;
public static class CryoMsiDb {
    [DllImport("msi.dll", CharSet = CharSet.Unicode)] static extern uint MsiOpenDatabase(string path, IntPtr persist, out uint db);
    [DllImport("msi.dll", CharSet = CharSet.Unicode)] static extern uint MsiDatabaseOpenView(uint db, string query, out uint view);
    [DllImport("msi.dll")] static extern uint MsiViewExecute(uint view, uint record);
    [DllImport("msi.dll")] static extern uint MsiViewFetch(uint view, out uint record);
    [DllImport("msi.dll")] static extern uint MsiRecordReadStream(uint record, uint field, byte[] buf, ref uint size);
    [DllImport("msi.dll")] static extern uint MsiCloseHandle(uint h);
    public static bool Save(string msi, string name, string outFile) {
        uint db, view, rec;
        if (MsiOpenDatabase(msi, IntPtr.Zero, out db) != 0) return false;
        try {
            if (MsiDatabaseOpenView(db, "SELECT `Data` FROM `Binary` WHERE `Name` = '" + name.Replace("'", "") + "'", out view) != 0) return false;
            try {
                if (MsiViewExecute(view, 0) != 0 || MsiViewFetch(view, out rec) != 0) return false;
                try {
                    using (var f = File.Create(outFile)) {
                        var buf = new byte[65536];
                        while (true) {
                            uint n = (uint)buf.Length;
                            if (MsiRecordReadStream(rec, 1, buf, ref n) != 0) return false;
                            if (n == 0) break;
                            f.Write(buf, 0, (int)n);
                        }
                    }
                    return true;
                } finally { MsiCloseHandle(rec); }
            } finally { MsiCloseHandle(view); }
        } finally { MsiCloseHandle(db); }
    }
}
'@
    }
    return [CryoMsiDb]::Save($path, $name, $out)
}

# ---- files -------------------------------------------------------------------
$tmp = Join-Path ([IO.Path]::GetTempPath()) ('cryo-verify-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $log = Join-Path $tmp 'admin.log'
    $p = Start-Process -FilePath msiexec.exe -ArgumentList @('/a', "`"$Msi`"", '/qn', "TARGETDIR=`"$tmp\image`"", '/l*v', "`"$log`"") -Wait -PassThru
    if ($p.ExitCode -ne 0) {
        Bad "administrative extract exited $($p.ExitCode) (log: $log)"
    } else {
        $agent = Get-ChildItem -LiteralPath "$tmp\image" -Recurse -Filter 'cryo-agent.exe' | Select-Object -First 1
        $restic = Get-ChildItem -LiteralPath "$tmp\image" -Recurse -Filter 'restic.exe' | Select-Object -First 1
        if (-not $agent -or -not $restic) {
            Bad 'cryo-agent.exe or restic.exe is not in the package'
        } else {
            if ($agent.Directory.Name -ne 'CryoBackup' -or $restic.DirectoryName -ne $agent.DirectoryName) {
                Bad "the binaries are not both in a CryoBackup folder ($($agent.DirectoryName), $($restic.DirectoryName))"
            }
            $h = Get-Sha256 $restic.FullName
            if ($h -eq $pin.exe_sha256) { Good "restic.exe is byte-identical to restic $($pin.version) upstream ($h)" }
            else { Bad "restic.exe in the package is $h, the pin is $($pin.exe_sha256)" }

            $bytes = [IO.File]::ReadAllBytes($agent.FullName)
            $text = [Text.Encoding]::ASCII.GetString($bytes)
            if ($text.Contains($pin.exe_sha256)) { Good 'cryo-agent.exe carries the restic pin' }
            else { Bad 'cryo-agent.exe does not carry the restic pin (PinnedResticSHA256 ldflag missing)' }
            $ver = & $agent.FullName version
            if ($LASTEXITCODE -eq 0 -and "$ver" -like 'cryo-agent *') { Good "cryo-agent.exe runs: $ver" } else { Bad "cryo-agent.exe version failed: $ver" }

            $dll = Join-Path $tmp 'cryoca.dll'
            if (Save-MsiBinary $Msi 'CryoCA' $dll) { Good 'cryoca.dll read from the Binary table' } else { Bad 'cryoca.dll could not be read from the Binary table' }
            $sa = Get-AuthenticodeSignature -LiteralPath $agent.FullName
            $sm = Get-AuthenticodeSignature -LiteralPath $Msi
            $sr = Get-AuthenticodeSignature -LiteralPath $restic.FullName
            $sd = $null; if (Test-Path -LiteralPath $dll) { $sd = Get-AuthenticodeSignature -LiteralPath $dll }
            if ($WantSign) {
                if ($sa.Status -eq 'Valid') { Good "cryo-agent.exe signature Valid ($($sa.SignerCertificate.Subject))" } else { Bad "cryo-agent.exe signature is $($sa.Status)" }
                if ($sd -and $sd.Status -eq 'Valid') { Good "cryoca.dll (as packed) signature Valid ($($sd.SignerCertificate.Subject))" } else { Bad "cryoca.dll (as packed) signature is $(if ($sd) { $sd.Status } else { 'missing' })" }
                if ($sm.Status -eq 'Valid') { Good "MSI signature Valid ($($sm.SignerCertificate.Subject))" } else { Bad "MSI signature is $($sm.Status)" }
                if ($sr.Status -eq 'Valid' -and $sa.Status -eq 'Valid' -and $sr.SignerCertificate.Thumbprint -eq $sa.SignerCertificate.Thumbprint) {
                    Bad 'restic.exe carries our signature: it must never be re-signed'
                }
            } else {
                Say "unsigned build: cryo-agent.exe $($sa.Status), cryoca.dll $(if ($sd) { $sd.Status } else { 'missing' }), MSI $($sm.Status) (expected NotSigned)"
                if ($sa.Status -eq 'Valid' -or $sm.Status -eq 'Valid') { Say 'note: a signature is present although signing was not requested' }
            }
            Say "restic.exe signature as shipped upstream: $($sr.Status)"
        }
    }
} finally {
    if ($script:failed -eq 0) { Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue }
    else { Say "extract kept for inspection: $tmp" }
}

# ---- report ------------------------------------------------------------------
if ($script:failed -gt 0) { Say "$($script:failed) check(s) failed"; exit 6 }
Say 'all checks passed'
exit 0
