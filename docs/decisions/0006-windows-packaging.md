# ADR 0006 - Windows packaging: MSI only, local signing, restic unsigned

**Date:** 2026-10-24 - **Status:** Accepted - **Decider:** operator

Amends feature rows r3 and r228 (M1).

## Decision

- **MSI only.** The agent ships as one WiX MSI (fixed UpgradeCode, major upgrade in place, silent `msiexec /i … /qn TOKEN=… SERVER=…`). There is no EXE bootstrapper (`EXE /S` in r3 is dropped); RMM deployment uses a PowerShell wrapper around `msiexec`, written to the project's Syncro/PowerShell rules.
- **Signing is local.** Release builds of `cryo-agent.exe` and the MSI are Authenticode-signed on the operator's build machine with the operator's signing script; the repository documents the step and ships a script that calls a signing command named by configuration, never a hardcoded identity. CI builds and validates the MSI unsigned and holds no signing credential.
- **restic.exe is not re-signed.** It is a third-party binary, shipped unmodified inside the signed MSI, pinned per agent release, and its SHA-256 is compiled into the agent and checked at every start (r208). r228 reads "agent and installer".

## Why

A bootstrapper EXE is a second artifact to sign, test and patch for no capability the MSI lacks. Keeping the signing identity off CI removes the most valuable credential from the most exposed system. Re-signing someone else's binary would claim authorship we do not have; the pinned hash already proves integrity.

## Consequences

- Tools that can only push EXEs run the PowerShell wrapper instead.
- A release cannot be cut from CI alone; the release checklist names the local signing step and a signature check (`Get-AuthenticodeSignature`) of both artifacts.
