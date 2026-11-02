# ADR 0012 - Operator settings live in the database once it exists

**Date:** 2026-10-31 - **Status:** Accepted - **Decider:** foreman (M2 build), recorded for the operator

Until M2 the display time zone and the board's refresh interval came from `config.json` alone, so changing either
meant editing a root-owned file and restarting the service. M2 adds an owner-editable /settings page (and
`PUT /api/v1/settings/{key}`). That raises a question: once a value has been changed in the console, which source
wins on the next start, the database or `config.json`?

## Decision

- **The database wins once it holds a value.** `init` stores the display time zone at first run, and either value
  is stored the first time an owner saves it on /settings. A stored value is authoritative from then on; while a
  key has never been stored, `config.json` supplies it. If the stored zone is not a usable IANA name, for example
  because the host's time-zone data changed, the console logs a warning at start and falls back to the
  config value, then UTC. It never refuses to start over a display preference.
- **Live, shared preferences.** One `DisplayPrefs` value is shared by the UI and the admin API, so the next
  request on either uses a changed zone. The live-update ticker follows a changed refresh interval within about
  15 s. Storage stays UTC everywhere (CLAUDE.md); only display changes.
- **Only two settings are editable in M2:** `display_timezone` and `refresh_interval_s`. The page shows the other
  settings the spec lists (retention, heartbeat, digest) read-only, with the milestone that will make them take
  effect. A setting that does nothing yet is not offered as editable.
- **Owner-only writes, audited per key.** Each changed key writes one audit row in the same transaction. A save
  that changes nothing writes nothing. A refused audit write refuses the change. Validation errors never echo the
  submitted value.

## Consequences

- An operator changes the zone or refresh rate from the console, without root and without a restart.
- Editing `config.json` after first run no longer changes these two values. The settings page states the source,
  and the README and config example must say so (follow-up in the M2 integrator ticket).
