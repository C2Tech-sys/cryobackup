# Windows agent MSI

The agent ships as one MSI, `cryobackup-agent.msi` (ADR 0006), built with WiX Toolset 5.0.2 (ADR 0007). It installs `cryo-agent.exe` and the pinned, unmodified `restic.exe` into `C:\Program Files\CryoBackup\`, creates the `CryoBackup` service, and can enroll the device during a silent install.

| File | What it is |
|---|---|
| `CryoBackup.wxs` | The package source. The UpgradeCode `{04B03054-481B-47AA-A586-4F275F1D5A51}` is fixed for ever. |
| `ca/cryoca.c` | The one custom-action DLL. It hands the install's request to `cryo-agent.exe` on stdin. |
| `build.ps1` | Builds the MSI. `make msi` runs it. |
| `sign.ps1` | The signing step (`CRYO_SIGN_COMMAND`). |
| `verify.ps1` | Checks a built MSI without installing it. It runs at the end of every build. |
| `restic.pin` | The restic version and its two SHA-256 values. This is the only place they are written. |
| `fetch-restic.ps1` | Downloads the pinned restic and checks it. |
| `eventlog/messages.mc`, `eventlog/build-messages.ps1` | The Event Log message table compiled into `cryo-agent.exe` (see **Event Log**). |
| `rmm/install-cryobackup.ps1` | The RMM wrapper: install, upgrade or enroll, idempotent. |
| `ci/smoke.ps1`, `ci/fakeconsole/` | The CI test that installs for real: install, upgrade, downgrade, uninstall, rollback. |
| `ci/wrapper-version.ps1` | Unit check of the wrapper's version rule (smoke.ps1 runs it). |

## Build

You need Windows, Go, a MinGW-w64 `gcc` targeting `x86_64-w64-mingw32` of major version **16** (the release build machine: WinLibs GCC 16.2.0 UCRT) or **15** (the CI runner, where the MSI is installed for real: MinGW-Builds GCC 15.2.0), the .NET SDK and git. `build.ps1` prints the compiler it found and refuses another target or major version (exit 2): `cryoca.dll` is only shipped from the compiler it was tested with. The accepted majors are `$GccMajors` at the top of `build.ps1`; adding one is a reviewed change, not a setting.

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\fetch-restic.ps1   # prints the restic.exe path
$env:CRYO_RESTIC_EXE = '<that path>'
powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\build.ps1          # or: make msi
```

The output is `dist\windows\cryobackup-agent.msi`. `build.ps1` does these steps in order:

1. **restic check.** It refuses any `restic.exe` whose SHA-256 is not `exe_sha256` in `restic.pin` (exit 3).
   - `zip_sha256` is restic's own published checksum. It is the `restic_0.19.1_windows_amd64.zip` line of the release's `SHA256SUMS`, at https://github.com/restic/restic/releases/download/v0.19.1/SHA256SUMS: `da948ad707ed690426473aaba2046cd61f8f90f6f0e7dab6be0d5796531de67d`.
   - `exe_sha256` is the hash of the single executable in that zip: `b0dd1fd21eea5d8fe1325f55f7118213c21f36de8a261e04c0624a5ab9fd7830`.
   - `fetch-restic.ps1` checks both.
2. **Agent build.** It compiles the Event Log message table (`eventlog\build-messages.ps1`: MinGW `windmc` and `windres`, from the same toolchain as `gcc`) into `cmd\cryo-agent\messages_windows_amd64.syso`, then builds `cryo-agent.exe` for windows/amd64, which links it, with `-X …/internal/agent.PinnedResticSHA256=<exe_sha256>`. The agent checks that hash at every start.
3. **Custom action.** It compiles `cryoca.dll` with `gcc -Werror` (linked with `msi` and `bcrypt`). The DLL is **not byte-reproducible**: its bytes depend on the exact gcc build and its runtime libraries, so two machines produce different, equally valid DLLs. What is reproducible is the source and the check that it was built with the documented compiler.
4. **First signing pass.** It signs `cryo-agent.exe` and `cryoca.dll` before they are packed, but only if `CRYO_SIGN_COMMAND` is set. It never signs `restic.exe`.
5. **Package.** It builds the MSI. `dotnet tool restore` pins WiX 5.0.2 from `.config/dotnet-tools.json`, and the build refuses any other WiX version.
6. **Second signing pass.** It signs the MSI, again only if `CRYO_SIGN_COMMAND` is set.
7. **Verify.** It runs `verify.ps1`, which takes the files out with `msiexec /a` and checks the following:
   - the UpgradeCode;
   - that `TOKEN` and the configure action's data are in `MsiHiddenProperties`, and that the action is `HideTarget`;
   - that `restic.exe` is byte-identical to the pin;
   - that `cryo-agent.exe` carries the pin and runs;
   - the signatures: with signing requested, `cryo-agent.exe`, `cryoca.dll` (read out of the MSI's Binary table, so the copy actually packed) and the MSI must all be `Valid`.

| Variable | Default | Meaning |
|---|---|---|
| `CRYO_RESTIC_EXE` | (required) | the pinned `restic.exe` |
| `CRYO_MSI_VERSION` | from git | `X.Y.Z[.N]` |
| `CRYO_MSI_MANUFACTURER` | `CryoBackup` | Publisher shown in Apps and Features |
| `CRYO_MSI_OUT` | `dist\windows` | output folder |
| `CRYO_SIGN_COMMAND` | unset (unsigned) | see below |
| `CRYO_WIX` | `dotnet tool run wix` | a `wix.exe`/`wix.dll` 5.0.2, for a machine without the .NET SDK |
| `CC` | `gcc` | C compiler for `cryoca.dll` |

The version comes from git:

- A tag `vX.Y.Z` on HEAD gives `X.Y.Z.0`.
- N commits after that tag give `X.Y.Z.N`.
- With no tag, the version is `0.0.0.N`, where N is the commit count.

Windows Installer compares only `X.Y.Z`. Two builds that differ only in N therefore replace each other in place (`AllowSameVersionUpgrades`), in either direction when run by hand with `msiexec`. The RMM wrapper compares all four fields (see the wrapper under **Install, upgrade, uninstall**). A release needs a new tag.

Exit codes of `build.ps1`:

| Code | Meaning |
|---|---|
| 0 | built and verified |
| 2 | a tool or input is missing |
| 3 | restic does not match the pin |
| 4 | a build step failed |
| 5 | signing failed |
| 6 | the MSI did not verify |

## Sign

Release builds are signed on the operator's machine (ADR 0006). CI never signs.

Set `CRYO_SIGN_COMMAND` to a program or `.ps1` script that does the following:

- takes one argument, the full path of a file;
- signs that file in place, with a timestamp;
- exits 0.

```powershell
$env:CRYO_SIGN_COMMAND = 'C:\signing\sign-one.ps1'   # yours: signtool + PFX/token, Azure Artifact Signing, SignPath, ...
powershell -NoProfile -ExecutionPolicy Bypass -File deploy\windows\build.ps1
```

The repository names no certificate, identity or account.

- **Order.** `build.ps1` calls the command for `cryo-agent.exe` and `cryoca.dll` before it packs them, then for the MSI.
- **Check after each file.** After every call it requires `Get-AuthenticodeSignature` to report `Valid`, and fails with exit 5 otherwise.
- **Final check.** `verify.ps1` checks both artifacts again. With `CRYO_SIGN_COMMAND` unset, the build prints an `UNSIGNED BUILD` banner and the result must not be shipped.
- **restic.exe.** It is never passed to the signing command; `sign.ps1` refuses it. Upstream restic is not Authenticode-signed, and its integrity rests on the pin.

## Install, upgrade, uninstall

Run these commands from an elevated prompt, or as SYSTEM from an RMM.

```powershell
# install and enroll
msiexec /i cryobackup-agent.msi /qn TOKEN=<enrollment token> SERVER=https://console.example:8443 /l*v C:\Windows\Temp\cryobackup.log
# install unenrolled (enroll later with "cryo-agent enroll", then "sc start CryoBackup")
msiexec /i cryobackup-agent.msi /qn /l*v C:\Windows\Temp\cryobackup.log
# upgrade: install the newer MSI the same way; TOKEN is not needed (the device stays enrolled)
msiexec /i cryobackup-agent-newer.msi /qn /l*v C:\Windows\Temp\cryobackup-upgrade.log
# uninstall (keeps C:\ProgramData\CryoBackup)
msiexec /x cryobackup-agent.msi /qn /l*v C:\Windows\Temp\cryobackup-remove.log
```

| Property | Meaning |
|---|---|
| `TOKEN` | Enrollment token. It is hidden: the MSI log shows `**********`. Give it together with `SERVER`, or leave both out. |
| `SERVER` | Console agent URL. It must be `https://`. |
| `CRYO_INSECURE_LAB=1` | Lab only. It allows an `http://` SERVER and turns off TLS verification. The agent warns about it on every start. |
| `CRYO_REPAIR_ACL=1` | If the existing state directory is untrusted, move it aside to `<name>.quarantine-…` and start fresh, as `cryo-agent install --repair-acl` does. The device must then be enrolled again. If the install fails after the move, see **Rollback**. |

**Enrollment retries.** A console that is busy (HTTP 429: it admits one enrollment per token at a time, so an RMM that pushes the MSI to a whole site at once gets the machines in a queue), temporarily unavailable (503, 502, 504) or not answering (a network error or timeout) is retried for up to 3 minutes, honouring the console's `Retry-After`, with the same enrollment nonce every time, so a retry never creates a second device. Each retry is a line in the MSI log (the status and the wait: no token, no path). A refusal a retry cannot change (a wrong or used token, a bad request, a TLS or pin failure) fails at once. `CRYO_ENROLL_RETRY_SECONDS` changes the budget, for `cryo-agent msi-configure` and `cryo-agent enroll`:

| `CRYO_ENROLL_RETRY_SECONDS` | Effect |
|---|---|
| unset | 3 minutes (the default) |
| `0` | one attempt, no retry (a console that is deliberately unreachable: tests, CI) |
| `1` to `600` | that many seconds |
| anything else | ignored, with a warning on stderr; the default applies |

The install's configure action runs inside the Windows Installer service, which does not see the environment of the shell that started `msiexec`. To change the budget for an MSI install, set the variable in the service's own environment (a `REG_MULTI_SZ` value `Environment` under `HKLM\SYSTEM\CurrentControlSet\Services\msiserver`, then restart the service); `deploy/windows/ci/smoke.ps1` does this for its deliberately failing installs. For `cryo-agent enroll` it is an ordinary environment variable of that process.

The token on an `msiexec` command line can be read by administrators while msiexec runs. The RMM wrapper avoids that: it calls the Windows Installer API in-process, so the token is on no command line at all. For a device that is already installed but not enrolled, the wrapper writes the token to the stdin of `cryo-agent.exe msi-configure`, the same channel the MSI uses; the wrapper never puts it in an environment variable or a file. If the RMM hands the token to the script in `CRYO_ENROLL_TOKEN` (instead of a script variable), that environment variable is the RMM's; the wrapper reads it once and clears it before it starts any child process.

```powershell
# RMM: set MsiPath, MsiSha256, ServerUrl, EnrollToken (secret) as script variables, or the CRYO_* environment variables
powershell -NoProfile -ExecutionPolicy Bypass -File install-cryobackup.ps1
```

The wrapper installs when the agent is missing, upgrades when the package is newer, and enrolls an installed but unenrolled device. Otherwise it reports and exits 0.

"Newer" is decided on all four version fields, `X.Y.Z.N`: a package whose four-part version is higher than the installed one is applied (an untagged `0.0.0.217` replaces `0.0.0.214`, as the MSI itself allows), a lower one is not applied ("no downgrade"; the MSI alone would accept a lower N with the same `X.Y.Z`), and an equal one is "already installed and current". A missing fourth field counts as 0. `ci/wrapper-version.ps1` checks this rule; the CI smoke test runs it.

Wrapper exit codes:

| Code | Meaning |
|---|---|
| 0 | done |
| 1 | not elevated, or not 64-bit Windows |
| 2 | bad input |
| 3 | the MSI failed |
| 4 | the result did not verify |
| 5 | enrollment failed |

**Logs.**

- In the `/l*v` log, every line the agent's custom action prints starts with `CryoBackup:`. A failed enrollment shows `enrollment failed, error class: network|tls|auth|console|state` and the agent's error, then `cryo-agent msi-configure failed with exit code N`.
- The running agent logs to `C:\ProgramData\CryoBackup\logs\agent.log` and to the Application Event Log, source `CryoBackup`. Event **1009** (warning) means the service found its own configuration drifted at start and re-applied it (see **Rollback**). Every `agent.log` line is flushed to disk as it is written, so the file's size in `dir` or Explorer is current while the service runs.

**Event Log.** Source `CryoBackup` in the Application log. Its message file (`EventMessageFile`) is `cryo-agent.exe` itself, which carries a message table with the text `%1` for every id below, so Event Viewer shows the agent's own line. (Earlier builds named `EventCreate.exe`, which has texts for ids 1 to 1000 only: every id below except 1000 showed "The description for Event ID … cannot be found". The MSI writes the new value on upgrade; `cryo-agent install` re-points an old registration.) Lines are fixed phrases and error classes, never paths.

| Id | Level | Meaning |
|---|---|---|
| 1000 | information | the agent started |
| 1001 | error | the restic binary failed verification; backups are refused |
| 1002 | error | a policy from the console was rejected as invalid |
| 1003 | information | the agent stopped |
| 1004 | error | refused to start on an unprotected or untrusted state directory |
| 1005 | warning | running with `--insecure-unprotected-state` (lab only) |
| 1006 | error | a state directory's owner or DACL changed while the agent ran; backups stop |
| 1007 | warning | `--repair-acl` quarantined an untrusted state directory and created a fresh one |
| 1008 | | retired (kept in the table so old events still render) |
| 1009 | warning | the service's own configuration had drifted and was re-applied |
| 1010 | information | a backup finished (success or partial) |
| 1011 | error | a backup failed, with its error class |
| 1012 | warning | a backup was deferred, with the reason |

To add an id: add it to `eventlog/messages.mc` and to `EventIDs` in `internal/agent/service`, run `eventlog\build-messages.ps1`, and commit the regenerated `cmd\cryo-agent\messages_windows_amd64.syso` with them (`go test ./cmd/cryo-agent ./internal/agent/service` checks that the three agree).

## What the MSI does

**Install**

1. **Launch conditions.** Before anything changes, the install requires 64-bit Windows 10 or Windows Server 2016 or later (build 10240 or later, read from the registry). It also refuses three combinations: TOKEN without SERVER, SERVER without TOKEN, and an `http://` SERVER without `CRYO_INSECURE_LAB=1`.
2. **Files.** It copies `cryo-agent.exe` and `restic.exe` to `C:\Program Files\CryoBackup\`. It registers the Event Log source `CryoBackup`. It creates the `CryoBackup` service (ServiceInstall), which runs as LocalSystem with `cryo-agent.exe run --state-dir C:\ProgramData\CryoBackup`.
3. **Configure.** It runs `cryo-agent.exe msi-configure` as SYSTEM, after the service exists and before it starts. That action does three things:
   - It prepares `C:\ProgramData\CryoBackup` exactly as `cryo-agent install` does: owner SYSTEM, a protected SYSTEM + Administrators DACL, the trust checks, and the HKLM untrusted-state verdict, which refuses the install.
   - It applies the service settings, with the same code as `cryo-agent install`: delayed automatic start, restart after 1, 5 and 15 minutes with the counter reset after a day, and a 120 s pre-shutdown budget.
   - If TOKEN and SERVER were given and the device is not enrolled yet, it enrolls.

   An already enrolled device keeps its identity. A TOKEN given to an enrolled device is ignored, and the log says so.
4. **Start.** It starts the service. An unenrolled device's service stops again at once: it refuses to run until it is enrolled, says so in its log and the Event Log, and does the same at every boot until it is enrolled.

**Upgrade.** A newer MSI removes the old version inside the same transaction (`MajorUpgrade` `afterInstallInitialize`). That stops the service, replaces both binaries, installs the service again and starts it. The state directory, and so the configuration, identity and journal, is not touched. A failed upgrade rolls back to the old version. An older MSI is refused: "Downgrading is refused".

**Uninstall.** It stops the service, waits for it, and removes the service, the Event Log source and `C:\Program Files\CryoBackup\`. It keeps `C:\ProgramData\CryoBackup`, so a later install picks up the same device (r231). To forget the device completely, delete that folder after uninstalling, from an elevated prompt.

**Rollback.** Windows Installer undoes the files, the service and the registry itself. The custom action undoes its own part:

- A state directory that this install created is removed whole, both when the enrollment fails and when anything later in the install fails. A failed first install therefore leaves no service and no state.
- A state directory that existed before the install is never removed, whatever made the install fail. Every install run gets its own random nonce (the immediate action `CryoNewNonce`, before the install script is built), passed to both the configure and the rollback action. Configure writes it into the marker `.msi-install-created` only when it created the directory itself; rollback removes the directory only when the marker holds its own run's nonce. A marker left by an earlier run that never committed, or no marker, keeps the directory. The commit action deletes the marker.
- If the configure action already enrolled the device and something after it failed, the console still has that device record. The token is consumed, so issue a new one.
- **A failed `CRYO_REPAIR_ACL=1` install does not put the quarantined tree back.** The repair moved the untrusted directory to `C:\ProgramData\CryoBackup.quarantine-<UTC stamp>-<8 hex>`. If the install then fails (for example the enrollment is refused), the rollback removes the fresh state directory the repair created and leaves the quarantine where it is. Moving it back would restore a directory that was refused as untrusted and would be refused again at the next install or service start, so the rollback does not try. The `/l*v` log says so (`it stays there (the rollback does not move it back ...)`) and names the quarantine. To recover: fix the cause shown in the log, install again with `TOKEN` and `SERVER` (`CRYO_REPAIR_ACL` is not needed again: the untrusted directory is gone and its verdict was cleared), which enrolls the device as new; then review the quarantine and delete it from an elevated prompt. The old device identity is not recovered: it was in the untrusted tree.
- **Service settings after a rolled-back upgrade.** Windows Installer restores the old service from its own record, which has no delayed start, restart-on-failure actions or pre-shutdown budget. The agent does not stay that way quietly: at every service start (it runs as LocalSystem) it compares its own SCM configuration with what `cryo-agent install` sets, re-applies anything missing, and writes a warning to `agent.log` and Event Log **1009** naming what it repaired. The repaired settings take effect from the next start. The CI smoke test rolls back an upgrade and asserts the repair.
- **The agent restores automatic (delayed) start at every service start, by design.** The same check treats any start type other than automatic with delayed start as drift. An administrator who sets the service to **Manual** and then starts it (`sc start CryoBackup`, `services.msc`) finds it set back to automatic delayed start, with Event Log **1009** (and the `agent.log` warning) recording that delayed automatic start was re-applied. A backup agent that stops starting by itself after a reboot is the silent failure this product exists to prevent, so the override is deliberate. To keep the agent from running, stop it and set it to **Disabled** (a disabled service never starts, so nothing re-applies the setting), or uninstall it.

### How the token is kept secret

An EXE custom action can receive data only on its command line, and other processes can read a command line. So the configure action is a DLL (`cryoca.dll`), which reads its data from the install session. The token takes this path:

```
TOKEN (hidden property)
  -> CryoConfigure (hidden property, HideTarget action: not written to the log)
  -> cryoca.dll
  -> pipe to cryo-agent.exe's stdin
  -> memory
```

- It is never on a command line, in an environment variable or in a file. (Outside the product: `msiexec ... TOKEN=...` typed by an operator puts it on msiexec's own command line; the RMM wrapper avoids even that.)
- `cryoca.dll` zeroes its buffers.
- `cryo-agent` prints no part of the token.
- The enrollment stores only the device credential the console returns, in the DPAPI store. The enrollment token itself is not stored anywhere.

The CI smoke test installs with a known token and then scans for it: the MSI logs, the state directory, the Windows Installer cache, the temp folders, and the Installer, Uninstall and service registry keys. The token must not appear in any of them.

### Why ServiceInstall plus a configure action

The two options were MSI-native service tables, or running `cryo-agent install` / `uninstall` as custom actions. The package uses the MSI-native tables for the service's life cycle, and the agent's own code for everything Windows Installer cannot do.

- **Native tables.** With ServiceInstall and ServiceControl, Windows Installer creates, stops, starts and deletes the service, and rolls each of those back. On a failed upgrade that also brings the old service back. Custom actions calling `install`/`uninstall` would have to rebuild all of that by hand: rollback actions for each step, plus conditions so that an upgrade's uninstall of the old version does not delete the service the new version needs.
- **Configure action.** What MSI cannot do is the state directory's ACL and trust checks, the HKLM verdict, and enrollment. The `MsiServiceConfig` tables, which would set delayed start, restart delays and the pre-shutdown time, are documented by the Windows Installer SDK as "not working as expected"; WiX warns about them. So `msi-configure` applies those settings by calling the same `service.Install` that `cryo-agent install` uses. The service ends up configured the same way whichever way it was installed.
- **No Util extension.** `WixToolset.Util`'s ServiceConfig can only express one restart delay, and it cannot set delayed start or pre-shutdown, so the package does not use it.

If a failed upgrade rolls back, the old service comes back as Windows Installer recorded it, without the delayed start and restart settings; its next start repairs them and says so (Event Log 1009, see **Rollback**).

## What the MSI does not do

- **No UI.** There are no dialogs. A double-click install runs with the basic progress bar and installs unenrolled.
- **No bootstrapper.** There is no EXE bootstrapper (ADR 0006). For an RMM that can only push EXEs, use the PowerShell wrapper.
- **No re-signing.** It does not sign or modify `restic.exe`.
- **No backups at install time.** The schedule comes from the console's policy after enrollment.
- **No self-update.** Upgrades are newer MSIs pushed by the RMM (architecture §6, M11).
- **No repair or reconfiguration of state.** A repair (`msiexec /f`) replaces missing files and the service but does not run the configure action. The service re-applies its own settings at its next start (Event Log 1009); `cryo-agent install` elevated does the same at once.
