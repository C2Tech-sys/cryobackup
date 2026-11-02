# sign.ps1 - the Authenticode signing step (ADR 0006), dot-sourced by build.ps1.
#
# The repository holds no signing identity, certificate path or account. The
# operator names a signing command in CRYO_SIGN_COMMAND: a program, or a .ps1
# script, that is called once per file with that file's full path as its only
# argument, signs the file in place (with a timestamp), and exits 0. What it
# wraps is the operator's business: signtool with a PFX or a hardware token,
# Azure Artifact Signing, SignPath. CI never sets it.
#
# Order (build.ps1): cryo-agent.exe and cryoca.dll are signed before they are
# packed into the MSI, then the MSI itself. restic.exe is never signed: it is a
# third-party binary shipped exactly as upstream publishes it, its integrity
# proven by the pinned SHA-256 (ADR 0006); Invoke-CryoSign refuses it.

# Invoke-CryoSign runs the signing command on one file and checks the result
# with Get-AuthenticodeSignature. It returns $true only for a Valid signature.
function Invoke-CryoSign($command, $file) {
    $leaf = Split-Path -Leaf $file
    if ($leaf -ieq 'restic.exe' -or $leaf -like 'restic_*') {
        Write-Host 'sign: refusing to sign restic.exe (ADR 0006: shipped unmodified, never re-signed)'
        return $false
    }
    Write-Host "sign: $leaf"
    if ($command -like '*.ps1') {
        & powershell -NoProfile -ExecutionPolicy Bypass -File $command $file
    } else {
        & $command $file
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Host "sign: the signing command exited $LASTEXITCODE for $leaf"
        return $false
    }
    $sig = Get-AuthenticodeSignature -LiteralPath $file
    if ($sig.Status -ne 'Valid') {
        Write-Host "sign: $leaf is not validly signed after signing: $($sig.Status) $($sig.StatusMessage)"
        return $false
    }
    Write-Host "sign: $leaf signed by $($sig.SignerCertificate.Subject)"
    return $true
}
