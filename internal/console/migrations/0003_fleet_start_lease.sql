-- 0003_fleet_start_lease
--
-- The fleet concurrency cap by console start-lease (ADR 0005, feature row r30):
--
--   a. storage_targets.max_concurrent_backups: the cap, per storage target.
--      0 = unlimited, the default, so an upgraded console changes nothing until
--      the operator sets it. ALTER TABLE ... ADD COLUMN is enough here (a new
--      column with a constant default and a CHECK that every existing row, 0,
--      passes); storage_targets is referenced by customers, so it is NOT rebuilt.
--      SQLite writes the added column into sqlite_master right after the last
--      column definition, before the table CHECK; replica/schema.sql carries the
--      same text so the two schemas compare equal (DH20).
--
--   b. runs.deferred_reason gains 'fleet_cap' (the console refused the start
--      lease). The CHECK cannot be altered in place, so runs is rebuilt with the
--      12-step procedure exactly as 0002 did
--      (https://www.sqlite.org/lang_altertable.html#otheralter):
--
--        1  foreign_keys: left ON. The runner executes this file inside BEGIN
--           IMMEDIATE, where PRAGMA foreign_keys is a no-op. Safe because NOTHING
--           REFERENCES runs (start_leases below does not either; a test asserts
--           it on the version-2 schema), so DROP TABLE runs has no child rows to
--           cascade into or orphan. runs only references devices, untouched.
--        2  (transaction)   provided by the runner.
--        3  remember the indexes: all eight idx_runs_* are recreated below,
--           verbatim. runs has no triggers and no views.
--        4  CREATE TABLE runs_new: the runs definition of replica/schema.sql with
--           the wider deferred_reason CHECK and nothing else changed.
--        5  INSERT INTO runs_new SELECT ...: every column by name, every row, in
--           rowid order.
--        6  DROP TABLE runs     (drops its indexes with it)
--        7  ALTER TABLE runs_new RENAME TO runs
--        8  recreate the indexes
--        9  (no views)
--       10  PRAGMA foreign_key_check: run by the migration runner before COMMIT.
--       11  (commit)           provided by the runner.
--       12  foreign_keys restored: never changed.
--
--   c. start_leases: one row per live lease, keyed by the agent's run id. A lease
--      lives 15 minutes, is renewed by the heartbeats of the running agent and
--      released by the upload of the closed run; expired rows are deleted when a
--      lease is asked for and by the heartbeat ring trim job.
--
-- The table texts below are copied from replica/schema.sql; a test compares the
-- schema this file produces with the one schema.sql produces (DH20).

ALTER TABLE storage_targets ADD COLUMN max_concurrent_backups INTEGER NOT NULL DEFAULT 0 CHECK (max_concurrent_backups >= 0);

CREATE TABLE runs_new (
  id               TEXT    PRIMARY KEY CHECK (length(id) = 26),                    -- agent-generated ULID = idempotency key (unique run id)
  device_id        TEXT    NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  origin           TEXT    NOT NULL DEFAULT 'agent' CHECK (origin IN ('agent','console')),
  kind             TEXT    NOT NULL CHECK (kind IN ('backup','restore','forget','prune','check','unlock','maintenance')),
  trigger_kind     TEXT    NOT NULL CHECK (trigger_kind IN ('schedule','catchup','wake','boot','manual','retry')),
  started_at       INTEGER NOT NULL CHECK (started_at >= 1000000000000),
  finished_at      INTEGER CHECK (finished_at IS NULL OR finished_at >= 1000000000000),
  received_at      INTEGER NOT NULL CHECK (received_at >= 1000000000000),
  clock_offset_ms  INTEGER NOT NULL DEFAULT 0,
  outcome          TEXT    CHECK (outcome IN ('success','partial','failed','cancelled','skipped','deferred','interrupted','abandoned')),
  restic_exit_code INTEGER,
  snapshot_id      TEXT    CHECK (snapshot_id IS NULL OR (length(snapshot_id) = 64 AND snapshot_id NOT GLOB '*[^0-9a-f]*')),
  stats            TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(stats)),
  data_added_bytes INTEGER CHECK (data_added_bytes IS NULL OR data_added_bytes >= 0),
  error_counts     TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(error_counts)),  -- {class: n}; never paths or messages
  error_class      TEXT    CHECK (error_class IN ('network','tls','auth','lock','vss','disk','permission','memory','repo_missing','wrong_password','storage','parse','guard_refused','internal','unknown','locked','missing_source','unsupported_file')),
  attempt          INTEGER NOT NULL DEFAULT 1 CHECK (attempt >= 1),
  deferred_reason  TEXT    CHECK (deferred_reason IN ('lock','gateway','battery','metered','window','conditions','fleet_cap')),
  policy_version   INTEGER CHECK (policy_version IS NULL OR policy_version >= 1),
  agent_version    TEXT,
  source_id        TEXT,                                                           -- non-device sources; NULL for device runs
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  CHECK (finished_at IS NULL OR finished_at >= started_at),
  CHECK ((outcome IS NULL) = (finished_at IS NULL)),                                -- open iff no outcome
  CHECK (outcome <> 'deferred' OR deferred_reason IS NOT NULL),
  CHECK (deferred_reason IS NULL OR COALESCE(outcome, '') = 'deferred'),
  CHECK (snapshot_id IS NULL OR kind = 'backup')
);

INSERT INTO runs_new (id, device_id, origin, kind, trigger_kind, started_at, finished_at, received_at, clock_offset_ms, outcome, restic_exit_code, snapshot_id, stats, data_added_bytes, error_counts, error_class, attempt, deferred_reason, policy_version, agent_version, source_id, created_at, updated_at)
  SELECT id, device_id, origin, kind, trigger_kind, started_at, finished_at, received_at, clock_offset_ms, outcome, restic_exit_code, snapshot_id, stats, data_added_bytes, error_counts, error_class, attempt, deferred_reason, policy_version, agent_version, source_id, created_at, updated_at
  FROM runs ORDER BY rowid;

DROP TABLE runs;
ALTER TABLE runs_new RENAME TO runs;

CREATE INDEX idx_runs_device_started   ON runs(device_id, started_at DESC);
CREATE INDEX idx_runs_started          ON runs(started_at DESC);
CREATE INDEX idx_runs_kind_started     ON runs(kind, started_at DESC);
CREATE INDEX idx_runs_outcome_started  ON runs(outcome, started_at DESC);
CREATE INDEX idx_runs_open             ON runs(device_id, started_at) WHERE outcome IS NULL;
CREATE INDEX idx_runs_last_success     ON runs(device_id, finished_at DESC) WHERE kind = 'backup' AND outcome = 'success';
CREATE INDEX idx_runs_last_backup_done ON runs(device_id, finished_at DESC) WHERE kind = 'backup' AND outcome IS NOT NULL;
CREATE INDEX idx_runs_maint_done       ON runs(device_id, kind, finished_at DESC) WHERE kind IN ('forget','prune','check') AND outcome IS NOT NULL;

CREATE TABLE start_leases (
  run_id             TEXT    PRIMARY KEY CHECK (length(run_id) = 26),                -- the agent's run id (ULID): one lease per run, the idempotency key
  device_id          TEXT    NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  storage_target_id  TEXT    NOT NULL REFERENCES storage_targets(id) ON DELETE CASCADE,  -- the target whose max_concurrent_backups the lease counts against
  granted_at         INTEGER NOT NULL CHECK (granted_at >= 1000000000000),
  lease_until        INTEGER NOT NULL,                                               -- live while > now; renewed by heartbeats of the running agent
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL,
  CHECK (lease_until > granted_at)
);
CREATE INDEX idx_start_leases_target_until ON start_leases(storage_target_id, lease_until);
CREATE INDEX idx_start_leases_device       ON start_leases(device_id);
