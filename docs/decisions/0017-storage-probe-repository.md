# ADR 0017 - The storage probe repository (banner C02)

**Date:** 2026-11-04 - **Status:** Accepted - **Decider:** foreman (M3-T2 delta round, verifier finding H1), recorded for the operator

Architecture §4.6 C02 says the storage probe is "`maintain` RPC: list prefix + `restic cat config` on the probe
repo", but no section defines the probe repository, its password or how it comes to exist. The first M3-T2 build
read a device repository instead and counted repo_missing (restic exit 10) as "storage reached": with an emptied
or re-pointed prefix every device answers exit 10 and C02 stayed green. This ADR fills the gap; it changes no
earlier decision.

## Decision

- **Location.** The probe repository is `<bucket>/<prefix>/probe` on the maintenance worker's storage target: the
  reserved name `probe` (architecture §7.2 "probe login": not a ULID, so it cannot collide with a device). It is
  the same location the gateway's probe login requests as `/probe/config` (C01), which therefore answers 200 once
  the repository exists (404 before; both stay healthy for C01).
- **Addressing.** The console asks the maintain RPC for `cat_config` with the reserved device id
  `adminpath.ProbeRepoID` (`"probe"`). No new RPC method: the worker answers that id from the probe repository;
  every other method answers `not_found` for it.
- **Steps.** "List prefix" is `restic list keys --no-lock` on the probe repository (a LIST under the prefix
  through the admin rclone path; `--no-lock` so the probe writes no lock object every minute), then
  `restic cat config` (a GET that also proves the key). Only a clean pair is a pass.
- **Password.** HKDF-SHA256 of the console master key with info `cryobackup/probe-repo/v1`, hex; never stored.
  The console binary's key unwrapper supplies it (`maintain.ProbeRepoKeyer`).
- **Creation, once.** When the list answers exit 10 and the settings row `storage_probe_repo` is absent, the worker
  creates the repository with `restic init` (storage-side only; nothing reaches the console host's disk, ADR
  0008) and records `{"created_at": ms}` in that row. A console with no enrolled device thus reaches a healthy C02.
  The first clean list + cat config also writes the row when it is missing, so a probe repository that already
  existed (a rebuilt database with the same key and prefix, an init cut short after its config was written) arms
  the guard below too.
- **Missing after creation is a failure.** With the row present, exit 10 is `repo_missing`: never a pass, never
  re-created. The console's prober counts it, like every restic verdict other than success, as a failed storage
  probe with its own class (two in a row raise C02). A worker it cannot reach (down, busy) is "not checked" (C02b,
  and C07 names the worker), never a pass.
- **Busy is bounded, not a timeout.** Each step of the probe waits at most `maintain.ProbeSlotWait` (3 s) for one of
  the worker's concurrency slots (shared with dump streams, browsing and the scheduler) and otherwise answers
  `busy`. Chosen over reserving a slot for probes because it changes nothing for the other callers and needs no
  new configuration; the cost is that a worker saturated for three prober intervals shows C02b until a slot frees.
- **One prober.** The console's probes run only while that process holds the `health-prober.probes` lease, renewed
  while any probe is in flight; a second console probes nothing (it shows the not-checked warnings), so two
  consoles never overlap probes or the first-boot `restic init`. The on-demand probe
  (`POST /api/v1/storage-targets/{id}/probe`, M3-INT) takes the same lease: it runs the gateway and storage
  probes now (or waits for the run already in flight), waits for their facts, and answers 409 when another
  console holds the lease. Its checks read only facts produced during the request; anything older is
  `not_checked`.

## Consequences

- An emptied bucket prefix, or a maintain worker re-pointed at another prefix, raises C02 within two probes.
- After an intentional move of the prefix, or a new master key (the probe password changes, so cat config answers
  wrong_password), the operator removes the old `<bucket>/<prefix>/probe` on storage and runs
  `cryo-console probe-repo reset --confirm` (a dry run without `--confirm`; it deletes only the settings row,
  writes one audit row and never touches storage). The next probe creates a fresh repository. Until then C02 stays
  raised, which is the intended loud default.
- A prefix that is wrong from the first day is not detected by this probe (the repository is created there); the
  device repositories' own checks (enrollment init, escrow verification) cover that case.
- One restic list and one cat config per minute run against storage, through one concurrency slot of the worker.
