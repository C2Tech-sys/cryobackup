# fetch-restic.ps1 - download the pinned restic release and check it.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\fetch-restic.ps1
#
# Downloads zip_url from restic.pin, checks the zip against restic's published
# SHA-256 (zip_sha256), extracts the one executable, checks it against
# exe_sha256, and prints its path for CRYO_RESTIC_EXE. Under GitHub Actions it
# also exports CRYO_RESTIC_EXE to the following steps. Idempotent: a file that
# is already there and matches is reused.
# Exit codes: 0 ready, 2 bad pin, 3 a checksum did not match, 4 download or
# extract failed.

$OutDir = $env:CRYO_RESTIC_DIR      # optional; default <repo>\dist\restic

. (Join-Path $PSScriptRoot 'pin.ps1')
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
if (-not $OutDir) { $OutDir = Join-Path $RepoRoot 'dist\restic' }

function Say($m) { Write-Host "fetch-restic: $m" }

# ---- discovery ---------------------------------------------------------------
$pin = Read-ResticPin (Join-Path $PSScriptRoot 'restic.pin')
if (-not $pin) { Say 'the restic pin file is missing or incomplete'; exit 2 }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$exe = Join-Path $OutDir $pin.exe_name
$zip = Join-Path $OutDir ([IO.Path]::GetFileName($pin.zip_url))

# ---- act ---------------------------------------------------------------------
if ((Test-Path -LiteralPath $exe) -and ((Get-Sha256 $exe) -eq $pin.exe_sha256)) {
    Say "already present and matching: $exe"
} else {
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -UseBasicParsing -Uri $pin.zip_url -OutFile $zip -ErrorAction Stop
    } catch { Say "download failed: $($_.Exception.Message)"; exit 4 }
    $zh = Get-Sha256 $zip
    if ($zh -ne $pin.zip_sha256) { Say "the zip is $zh, restic's published SHA-256 is $($pin.zip_sha256): refusing it"; exit 3 }
    Say "zip matches restic's published SHA-256 ($zh)"
    $x = Join-Path $OutDir ('x-' + [guid]::NewGuid().ToString('N'))
    try {
        Expand-Archive -LiteralPath $zip -DestinationPath $x -ErrorAction Stop
        Copy-Item -LiteralPath (Join-Path $x $pin.exe_name) -Destination $exe -Force -ErrorAction Stop
    } catch { Say "extract failed: $($_.Exception.Message)"; exit 4 }
    finally { Remove-Item -LiteralPath $x -Recurse -Force -ErrorAction SilentlyContinue }
}

# ---- verify ------------------------------------------------------------------
$eh = Get-Sha256 $exe
if ($eh -ne $pin.exe_sha256) { Say "the executable is $eh, the pin is $($pin.exe_sha256)"; exit 3 }

# ---- report ------------------------------------------------------------------
Say "restic $($pin.version) ready: $exe"
if ($env:GITHUB_ENV) { Add-Content -LiteralPath $env:GITHUB_ENV -Value "CRYO_RESTIC_EXE=$exe" }
exit 0
