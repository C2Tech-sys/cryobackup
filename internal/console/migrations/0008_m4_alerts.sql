-- 0008_m4_alerts
--
-- M4 (ADR 0020, decision 6): the console remembers that an alarm is open and that mail went
-- out for it. States are computed on read, so without this table nothing records "this
-- failure was already mailed" and every evaluator pass would mail it again.
--
--   alarm_episodes   one row per OPEN alarm of one subject (a device or a customer) and one
--                    alarm type. opened_at is when the console first saw the alarm; state_since
--                    is the state engine's own "since" (it only approximates the first failure
--                    of a streak, and may move: the episode, not the since value, decides
--                    whether a mail is due). notified_at is when the alert went out (NULL = not
--                    yet). closed_at is when the alarm was gone (NULL = still open), and
--                    recovery_notified_at when the recovery notice went out. A recovery notice
--                    only follows a close (CHECK). alarm_type holds the state name (FAILED,
--                    STALE, MISSING, ...); the vocabulary lives in the store, not in a CHECK,
--                    so a later alarm needs no table rebuild.
--
-- At most one OPEN episode per subject and alarm type (the two partial unique indexes, like
-- alarm_acks); an alarm that comes back after it closed opens a new episode. The other
-- indexes serve the three questions the alerting jobs ask: which episodes are open, which are
-- still unmailed, and which closed ones still owe a recovery notice. device_id and
-- customer_id each lead a full index as well, so deleting a device or a customer cascades
-- without a scan.
--
-- notification_rules, notification_log and alarm_acks (0001) are used as designed; this
-- migration changes none of them.
--
-- digest_enabled (ADR 0020 decision 8: the digest goes out every day, so "on" is the default)
-- is seeded here so the settings page shows its value instead of "not set". INSERT OR IGNORE
-- leaves a value an operator already stored alone. replica/schema.sql carries the same table
-- and indexes (DH20).

CREATE TABLE alarm_episodes (
  id                    TEXT    PRIMARY KEY CHECK (length(id) = 26),
  subject_type          TEXT    NOT NULL CHECK (subject_type IN ('device','customer')),
  device_id             TEXT    REFERENCES devices(id) ON DELETE CASCADE,
  customer_id           TEXT    REFERENCES customers(id) ON DELETE CASCADE,
  alarm_type            TEXT    NOT NULL CHECK (length(alarm_type) BETWEEN 1 AND 40),                 -- the state name: FAILED, STALE, MISSING, ...
  opened_at             INTEGER NOT NULL CHECK (opened_at >= 1000000000000),
  state_since           INTEGER NOT NULL CHECK (state_since >= 1000000000000),                        -- the state engine's "since" at the time the episode opened
  notified_at           INTEGER CHECK (notified_at IS NULL OR notified_at >= 1000000000000),          -- NULL = the alert has not gone out yet
  closed_at             INTEGER CHECK (closed_at IS NULL OR closed_at >= 1000000000000),              -- NULL = the alarm is still open
  recovery_notified_at  INTEGER CHECK (recovery_notified_at IS NULL OR (closed_at IS NOT NULL AND recovery_notified_at >= 1000000000000)),
  created_at            INTEGER NOT NULL,
  updated_at            INTEGER NOT NULL,
  CHECK ((subject_type = 'device' AND device_id IS NOT NULL AND customer_id IS NULL)
      OR (subject_type = 'customer' AND customer_id IS NOT NULL AND device_id IS NULL))
);
CREATE INDEX idx_alarm_episodes_device     ON alarm_episodes(device_id);
CREATE INDEX idx_alarm_episodes_customer   ON alarm_episodes(customer_id);
CREATE UNIQUE INDEX uq_alarm_episodes_open_device   ON alarm_episodes(device_id, alarm_type)   WHERE closed_at IS NULL AND device_id IS NOT NULL;
CREATE UNIQUE INDEX uq_alarm_episodes_open_customer ON alarm_episodes(customer_id, alarm_type) WHERE closed_at IS NULL AND customer_id IS NOT NULL;
CREATE INDEX idx_alarm_episodes_open       ON alarm_episodes(opened_at) WHERE closed_at IS NULL;
CREATE INDEX idx_alarm_episodes_unnotified ON alarm_episodes(opened_at) WHERE closed_at IS NULL AND notified_at IS NULL;
CREATE INDEX idx_alarm_episodes_recovery_due ON alarm_episodes(closed_at) WHERE closed_at IS NOT NULL AND notified_at IS NOT NULL AND recovery_notified_at IS NULL;

INSERT OR IGNORE INTO settings (key, value, created_at, updated_at)
  VALUES ('digest_enabled', 'true', CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000);
