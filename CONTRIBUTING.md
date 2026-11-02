# Contributing to CryoBackup

Thanks for looking. The project is early (Phase 1); read `CLAUDE.md` for the conventions and `replica/architecture.md` for the build contract before opening a pull request.

- **Build:** Go 1.26 or newer, `make all`. No cgo; the console's SQLite is pure Go.
- **Tests:** every package ships table-driven tests; the state engine (`internal/state`) must pass its full case list before any UI change is reviewed.
- **Commits:** small, one concern each, imperative subject line.
- **Decisions:** anything that changes an ADR gets a new ADR in `docs/decisions/`.
- **Contributor agreement:** contributions require a contributor licence agreement so the project can offer dual licensing later. The agreement text is being prepared; until it is published, pull requests from outside the maintainers are reviewed but not merged.
- **Never commit** secrets, customer names, hostnames or anything operator-specific. Fixtures use fictional names.
