Superseded in part by ADR 0004 (Go minimum)

# ADR 0003 - Toolchain and pinned third-party binaries

**Date:** 2026-10-21 - **Status:** Accepted - **Decider:** foreman under ADR 0002

## Decision

- **Go 1.25** minimum (`go.mod`), built and tested on the newest stable toolchain in CI (`actions/setup-go` with `go-version-file`). `CGO_ENABLED=0` everywhere; cross-compilation from any host is a requirement, so no cgo dependency is accepted (SQLite is `modernc.org/sqlite`).
- **restic 0.19.1** and **rclone 1.75.1** are the pinned third-party binaries for this phase. The agent bundles `restic.exe` as a separate MSI component and verifies its SHA-256 at start; the gateway image pins rclone. Upgrading either is a release of CryoBackup with the engine sheet re-checked.
- **Lint:** golangci-lint v2 configuration in `.golangci.yml` (standard set plus gosec, misspell, goimports).
- **CI:** GitHub Actions; `vet` + `test -race` on Ubuntu and Windows, then `make cross` artifacts. Signing and MSI packaging jobs are added in M1 (WiX v5 on a Windows runner; the signing command is pluggable, never a vendor).
- **Layout:** `cmd/` per binary, `internal/` for everything else, `web/` embedded assets, `deploy/` operational files, `test/` lab scripts, `docs/decisions/` ADRs, `replica/` the Replica-pipeline artefacts.

## Why

One language and one module were decided in ADR 0002 D1; this ADR fixes the versions so a stranger can reproduce a build. Go 1.25 rather than the newest release keeps distribution packagers in range. The restic version matches the one the engine capability sheet verified (`docs/recon-notes/engine_capabilities.md`), so every flag in `internal/restic` has a citation.
