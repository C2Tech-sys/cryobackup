# 09-selfbackup.ps1 - item 9: restic backup of a small staging directory through
# the rclone: backend with the maintain worker's options (--retry-lock 30s,
# --no-cache, -o rclone.program=..., -o rclone.args=serve restic --stdio), into a
# repository under a SEPARATE prefix (<prefix>-console, decision C3); forget
# --prune with the C4 keep rules; check --read-data; then a restore into a bare
# directory with nothing but the rclone config and the repository password.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
$SelfRepo = "rclone:admin:$(((Join-Path $Root "store\$Bucket\$Prefix-console") -replace '\\','/'))"
$Stage = Join-Path $Root 'selfstage'
$Bare = Join-Path $Root 'bare-host'
Write-Note 'ITEM 9 start (self-backup repository under <prefix>-console, rclone: backend, local remote)'
$opts = @('--retry-lock', '30s') + (Get-AdminOpts)
$es = @{
    RESTIC_REPOSITORY = $SelfRepo; RESTIC_PASSWORD = (Get-Secret 'self-password'); RCLONE_CONFIG = (Join-Path $Secrets 'rclone.conf')
    RESTIC_CACHE_DIR = $null; RESTIC_CACERT = $null; TEMP = (Join-Path $Root 'tmp'); TMP = (Join-Path $Root 'tmp')
}
function New-Stage([int]$Round) {
    if (Test-Path -LiteralPath $Stage) { Remove-Item -LiteralPath $Stage -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $Stage | Out-Null
    $b = New-Object byte[] (2MB); [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
    [IO.File]::WriteAllBytes("$Stage\console.db", $b)
    [IO.File]::WriteAllText("$Stage\config.json", "{`"round`": $Round}", $Utf8NoBom)
    $sha = (Get-FileHash -LiteralPath "$Stage\console.db" -Algorithm SHA256).Hash.ToLower()
    [IO.File]::WriteAllText("$Stage\manifest.json", "{`"round`": $Round, `"db_sha256`": `"$sha`", `"db_bytes`": $($b.Length)}", $Utf8NoBom)
    return $sha
}

$r = Invoke-Restic 'item9 cat config (expect 10: no repository)' ($opts + @('cat', 'config')) $es
if ($r.Rc -eq 10) { $r = Invoke-Restic 'item9 init via rclone:' ($opts + @('init')) $es }
$shas = @()
for ($round = 1; $round -le 3; $round++) {
    $shas += New-Stage $round
    $r = Invoke-Restic "item9 backup round $round via rclone:" ($opts + @('backup', '--host', 'console', '--json', '--', $Stage)) $es
    $sum = ($r.Out -split "`r?`n" | Where-Object { $_ -match '"message_type":"summary"' } | Select-Object -Last 1)
    Write-Note "item9 round $round summary: $sum"
}
$r = Invoke-Restic 'item9 snapshots' ($opts + @('snapshots', '--json')) $es
Write-Note "item9 snapshots before forget: $((ConvertFrom-Json -InputObject $r.Out).Count)"
$r = Invoke-Restic 'item9 forget --prune with C4 rules (keep-within 7d, daily 30, monthly 12)' ($opts + @('forget', '--prune', '--keep-within', '7d', '--keep-daily', '30', '--keep-monthly', '12')) $es
$r = Invoke-Restic 'item9 forget --prune --keep-last 1 (forces removal + prune work)' ($opts + @('forget', '--prune', '--keep-last', '1')) $es
$r = Invoke-Restic 'item9 snapshots after forget' ($opts + @('snapshots', '--json')) $es
Write-Note "item9 snapshots after forget --keep-last 1: $((ConvertFrom-Json -InputObject $r.Out).Count)"
$r = Invoke-Restic 'item9 check --read-data' ($opts + @('check', '--read-data')) $es

# bare-host restore: a fresh environment holding ONLY the rclone config, the
# password, PATH/SystemRoot; new empty TEMP; no cache directory exists.
if (Test-Path -LiteralPath $Bare) { Remove-Item -LiteralPath $Bare -Recurse -Force }
New-Item -ItemType Directory -Force -Path "$Bare\tmp", "$Bare\out" | Out-Null
$pwFile = Join-Path $Bare 'password.txt'
Copy-Item -LiteralPath (Join-Path $Secrets 'self-password') -Destination $pwFile
$confCopy = Join-Path $Bare 'rclone.conf'
Copy-Item -LiteralPath (Join-Path $Secrets 'rclone.conf') -Destination $confCopy
$bareEnv = @{}
foreach ($k in @([Environment]::GetEnvironmentVariables().Keys)) { $bareEnv[[string]$k] = $null }   # drop everything ...
$bareEnv.PATH = "$env:SystemRoot\system32;$env:SystemRoot"; $bareEnv.SystemRoot = $env:SystemRoot  # ... keep the minimum
$bareEnv.RCLONE_CONFIG = $confCopy; $bareEnv.RESTIC_PASSWORD_FILE = $pwFile; $bareEnv.TEMP = "$Bare\tmp"; $bareEnv.TMP = "$Bare\tmp"
$bareEnv.USERPROFILE = "$Bare"; $bareEnv.LOCALAPPDATA = "$Bare\tmp"; $bareEnv.APPDATA = "$Bare\tmp"
$r = Invoke-Restic 'item9 bare-host restore latest' ($opts + @('-r', $SelfRepo, 'restore', 'latest', '--target', "$Bare\out")) $bareEnv
$restored = Get-ChildItem -LiteralPath "$Bare\out" -Recurse -File -Filter 'console.db' | Select-Object -First 1
$got = if ($restored) { (Get-FileHash -LiteralPath $restored.FullName -Algorithm SHA256).Hash.ToLower() } else { '(none)' }
Write-Note "item9 bare-host restore: console.db sha256 equal to round-3 staging = $($got -eq $shas[-1]); restored files: $(@(Get-ChildItem -LiteralPath "$Bare\out" -Recurse -File).Count); cache dirs created under bare profile: $(@(Get-ChildItem -LiteralPath "$Bare\tmp" -Recurse -Directory -ErrorAction SilentlyContinue | Where-Object Name -match 'restic').Count)"
Remove-Item -LiteralPath $Stage -Recurse -Force
