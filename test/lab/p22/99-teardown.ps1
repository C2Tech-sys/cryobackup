# 99-teardown.ps1 - stop the gateway, remove the subst drive and delete the rig
# directory (secrets, repository, sample data, logs). Set P22_KEEP_LOGS=1 to copy
# log\calls.md next to the rig before deleting it (it holds no secret: every
# logged line went through ConvertTo-Generic).
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'rig.ps1')
Stop-Gateway
# any rclone still serving this rig's store (a leftover from an interrupted step)
Get-CimInstance Win32_Process -Filter "Name='rclone.exe'" | Where-Object { $_.CommandLine -like "*$Root*" } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }
$drive = Get-RigDrive
if ($drive) { & subst.exe $drive /D | Out-Null }
if ($env:P22_KEEP_LOGS -eq '1' -and (Test-Path -LiteralPath $CallLog)) { Copy-Item -LiteralPath $CallLog -Destination "$Root-calls.md" -Force }
# long paths inside the rig: let cmd's rd handle them through the \\?\ prefix
& cmd.exe /c rd /s /q "\\?\$Root"
Write-Host "listener on $Port left: $(Test-Port $Port); rig directory left: $(Test-Path -LiteralPath $Root); subst: $((& subst.exe | Out-String).Trim())"
