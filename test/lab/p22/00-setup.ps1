# 00-setup.ps1 - build the P22-S1 rig: secrets, TLS pair, htpasswd, rclone config,
# subst drive with sample data, the gateway, the device repository and the first
# backup. Idempotent: each part is skipped when already present.
#
#   $env:P22_RIG_ROOT = 'D:\scratch\p22'   # optional; default %TEMP%\cryo-p22-rig
#   powershell -NoProfile -ExecutionPolicy Bypass -File test\lab\p22\00-setup.ps1
#
# Needs: restic 0.19.1 and rclone 1.75.1 (PATH or CRYO_RESTIC / CRYO_RCLONE), Go
# (module cache already holding golang.org/x/crypto; GOPROXY=off, no download),
# openssl (Git for Windows ships one). No elevation.

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')

$OpenSsl   = if ($env:P22_OPENSSL) { $env:P22_OPENSSL } else { 'C:\Program Files\Git\mingw64\bin\openssl.exe' }
$BigBytes  = 1GB        # the large file
$FlatCount = 2000       # one folder with this many files (browse page test)
$DirCount  = 18         # plus this many folders ...
$PerDir    = 1000       # ... of this many files: 2000 + 18*1000 = 20000 small files

# ---- secrets (random, never printed) ------------------------------------------
function New-RandomHex([int]$Bytes) {
    $b = New-Object byte[] $Bytes
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
    -join ($b | ForEach-Object { $_.ToString('x2') })
}
& icacls.exe $Secrets /inheritance:r /grant:r "$($env:USERNAME):(OI)(CI)F" | Out-Null
foreach ($n in @('gateway-password', 'gateway-password-2', 'repo-password', 'self-password')) {
    $f = Join-Path $Secrets $n
    if (-not (Test-Path -LiteralPath $f)) { [IO.File]::WriteAllText($f, (New-RandomHex 24), $Utf8NoBom) }
}

# ---- TLS pair for 127.0.0.1 ---------------------------------------------------
$crt = Join-Path $Secrets 'gateway.crt'; $key = Join-Path $Secrets 'gateway.key'
if (-not (Test-Path -LiteralPath $crt)) {
    $r = Invoke-Rig -Label 'setup openssl' -Exe $OpenSsl -ArgList @('req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
        '-nodes', '-keyout', $key, '-out', $crt, '-days', '3', '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1')
    if ($r.Rc -ne 0) { throw 'openssl failed' }
}

# ---- htpasswd: two device logins, hashed as the console does ------------------
$ht = Join-Path $Secrets 'htpasswd'
if (-not (Test-Path -LiteralPath $ht)) {
    $lines = @()
    foreach ($pair in @(@($DeviceId, 'gateway-password'), @('spike-dev-02', 'gateway-password-2'))) {
        $o = Join-Path $Root 'tmp-ht.txt'
        $env:GOFLAGS = '-mod=readonly'; $env:GOPROXY = 'off'
        $p = Start-Process -FilePath 'go' -ArgumentList @('run', './test/lab/p22/htpasswd.go', $pair[0]) -WorkingDirectory $RepoDir `
            -RedirectStandardInput (Join-Path $Secrets $pair[1]) -RedirectStandardOutput $o -NoNewWindow -Wait -PassThru
        Remove-Item Env:GOFLAGS, Env:GOPROXY
        if ($p.ExitCode -ne 0) { throw 'htpasswd helper failed' }
        $lines += (Get-Content -LiteralPath $o -TotalCount 1)
        Remove-Item -LiteralPath $o
    }
    [IO.File]::WriteAllText($ht, (($lines -join "`n") + "`n"), $Utf8NoBom)
}

# ---- rclone config: [store] for the gateway, [admin] for the maintain path ------
$rc = Join-Path $Secrets 'rclone.conf'
if (-not (Test-Path -LiteralPath $rc)) {
    [IO.File]::WriteAllText($rc, "[store]`ntype = local`n`n[admin]`ntype = local`n", $Utf8NoBom)
}
New-Item -ItemType Directory -Force -Path $RepoTop | Out-Null

# ---- subst drive (stands in for a volume root such as C:) ----------------------
New-Item -ItemType Directory -Force -Path $DriveDir | Out-Null
$drive = Get-RigDrive
$substOut = (& subst.exe | Out-String)
if (-not $drive -or ($substOut -notmatch [regex]::Escape("$($drive)\:"))) {
    $used = (Get-PSDrive -PSProvider FileSystem).Name
    $free = @('R','S','T','U','V','W','Y','Q','P','O') | Where-Object { $used -notcontains $_ -and ($substOut -notmatch "^$($_):") } | Select-Object -First 1
    if (-not $free) { throw 'no free drive letter' }
    & subst.exe "$($free):" $DriveDir
    if ($LASTEXITCODE -ne 0) { throw 'subst failed' }
    Set-Content -LiteralPath (Join-Path $State 'drive.txt') -Value "$($free):"
    $drive = "$($free):"
}
Write-Host "rig drive $drive"

# ---- sample data ----------------------------------------------------------------
$data = Join-Path $DriveDir 'data'          # real path; the same tree is $drive\data
function Write-Small([string]$Path, [string]$Text) {
    $d = Split-Path -Parent $Path
    if (-not (Test-Path -LiteralPath $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
    [IO.File]::WriteAllText($Path, $Text, $Utf8NoBom)
}
if (-not (Test-Path -LiteralPath (Join-Path $State 'data.done'))) {
    # nested folders
    Write-Small "$data\nested\top.txt" 'top'
    Write-Small "$data\nested\a\sibling.txt" 'sibling'
    Write-Small "$data\nested\a\b\c\deep.txt" 'deep'
    # parent-metadata probe (item 2): parent\keep\* exist before a restore
    Write-Small "$data\parent\keep\one.txt" 'one'
    Write-Small "$data\parent\keep\two.txt" 'two'
    # long path (> 260 characters), made with the \\?\ prefix
    $seg = 'L' * 60
    $long = "\\?\$data\long\$seg\$seg\$seg\$seg"
    [IO.Directory]::CreateDirectory($long) | Out-Null
    [IO.File]::WriteAllText("$long\$('f' * 70).txt", 'long path file', $Utf8NoBom)
    # names with [ ] $ % ! spaces and non-ASCII ( * and ? are not legal in NTFS names )
    $uni = 'caf' + [char]0x00E9 + ' ' + [char]0x00FC + [char]0x00F1 + [char]0x00EF + ' ' + [char]0x65E5 + [char]0x672C
    foreach ($n in @('bracket [ab].txt', 'a.txt', 'b.txt', 'x]y.txt', '[x].txt', 'dollar $P22VAR.txt', 'p22value.txt',
                     'percent %P22VAR% 100%.txt', 'bang !x!.txt', 'space in name.txt', "$uni.txt", 'brace {a,b}.txt', 'back`tick.txt')) {
        Write-Small (Join-Path "$data\names" $n) "name: $n"
    }
    # custom ACL: protected DACL, explicit entries only
    Write-Small "$data\acl\custom.txt" 'custom acl'
    & icacls.exe "$data\acl\custom.txt" /inheritance:r /grant:r "$($env:USERNAME):(F)" '*S-1-1-0:(R)' | Out-Null
    New-Item -ItemType Directory -Force -Path "$data\acl\customdir" | Out-Null
    & icacls.exe "$data\acl\customdir" /inheritance:r /grant:r "$($env:USERNAME):(OI)(CI)(F)" '*S-1-5-32-545:(OI)(CI)(RX)' | Out-Null
    Write-Small "$data\acl\customdir\inner.txt" 'inherits from customdir'
    Write-Small "$data\acl\plain.txt" 'inherited acl only'
    # alternate data stream
    Write-Small "$data\ads\withstream.txt" 'main stream'
    Set-Content -LiteralPath "$data\ads\withstream.txt" -Stream 'extra' -Value 'alternate stream data'
    # empty folder
    New-Item -ItemType Directory -Force -Path "$data\empty" | Out-Null
    # 20 000 small files
    New-Item -ItemType Directory -Force -Path "$data\many\flat" | Out-Null
    for ($i = 0; $i -lt $FlatCount; $i++) { [IO.File]::WriteAllText(("$data\many\flat\f{0:D5}.txt" -f $i), "flat $i", $Utf8NoBom) }
    for ($d = 1; $d -le $DirCount; $d++) {
        $dd = "$data\many\d{0:D2}" -f $d
        New-Item -ItemType Directory -Force -Path $dd | Out-Null
        for ($i = 0; $i -lt $PerDir; $i++) { [IO.File]::WriteAllText(("$dd\f{0:D5}.txt" -f $i), "d$d $i", $Utf8NoBom) }
    }
    # 1 GiB random file
    New-Item -ItemType Directory -Force -Path "$data\big" | Out-Null
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    $buf = New-Object byte[] (4MB)
    $fs = [IO.File]::Create("$data\big\big.bin")
    for ($w = 0; $w -lt ($BigBytes / 4MB); $w++) { $rng.GetBytes($buf); $fs.Write($buf, 0, $buf.Length) }
    $fs.Close()
    New-Item -ItemType File -Path (Join-Path $State 'data.done') | Out-Null
}

# ---- gateway, repository, first backup -------------------------------------------
Stop-Gateway
Start-Gateway | Out-Null
$e = Get-GatewayEnv
$r = Invoke-Restic 'setup cat config' @('cat', 'config') $e
if ($r.Rc -eq 10) { $r = Invoke-Restic 'setup init (through gateway)' @('init') $e; if ($r.Rc -ne 0) { throw 'init failed' } }
elseif ($r.Rc -ne 0) { throw "cat config exit $($r.Rc)" }
if (-not (Test-Path -LiteralPath (Join-Path $State 'backup1.done'))) {
    $r = Invoke-Restic 'setup backup #1 (through gateway)' @('backup', '--host', $DeviceId, '--json', '--', "$drive\data") $e
    if ($r.Rc -ne 0) { throw 'first backup failed' }
    $sum = ($r.Out -split "`r?`n" | Where-Object { $_ -match '"message_type":"summary"' } | Select-Object -Last 1) | ConvertFrom-Json
    Set-Content -LiteralPath (Join-Path $State 'snap-full.txt') -Value $sum.snapshot_id
    New-Item -ItemType File -Path (Join-Path $State 'backup1.done') | Out-Null
}
Write-Host 'setup done (gateway left running; 99-teardown.ps1 stops it)'
