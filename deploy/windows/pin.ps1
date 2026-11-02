# pin.ps1 - read deploy\windows\restic.pin (dot-sourced by the other scripts).

# Read-ResticPin returns the pin as a hashtable (version, zip_url, zip_sha256,
# exe_name, exe_sha256), or $null when the file is missing, a value is absent,
# or a hash is not 64 lower-case hex digits.
function Read-ResticPin($path) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { return $null }
    $pin = @{}
    foreach ($line in Get-Content -LiteralPath $path) {
        $t = $line.Trim()
        if ($t -eq '' -or $t.StartsWith('#')) { continue }
        $i = $t.IndexOf('=')
        if ($i -lt 1) { return $null }
        $pin[$t.Substring(0, $i)] = $t.Substring($i + 1)
    }
    foreach ($k in @('version', 'zip_url', 'zip_sha256', 'exe_name', 'exe_sha256')) {
        if (-not $pin[$k]) { return $null }
    }
    foreach ($k in @('zip_sha256', 'exe_sha256')) {
        if ($pin[$k] -cnotmatch '^[0-9a-f]{64}$') { return $null }
    }
    return $pin
}

# Get-Sha256 returns a file's SHA-256 as lower-case hex.
function Get-Sha256($path) {
    return (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
}
