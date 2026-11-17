-- 0007_adr19_purge
--
-- Purging a retired device (ADR 0019). A purge runs in two parts: the console marks
-- the device (this migration's columns), a job removes the repository from storage,
-- and only then is the device row deleted. Until then the device shows as "purging".
--
--   devices.purge_requested_at       when an owner asked for the purge (epoch ms, like every
--                                    time here). NOT NULL means "purging". Only a retired
--                                    device can carry it: the CHECK holds that rule below the
--                                    store, so no code path can mark an active device. The
--                                    same CHECK stops anyone un-retiring a purging device.
--   devices.purge_requested_by       the owner who asked; SET NULL when that account is
--                                    deleted, like devices.paused_by. The audit row keeps the
--                                    name.
--   devices.purge_reason             the reason given (one line, no paths); kept with the row.
--   devices.purge_last_error_class   class of the last failed storage attempt (a fixed word,
--                                    never a message); NULL = no failure outstanding.
--   devices.purge_last_attempt_at    when the job last tried (epoch ms).
--
-- The last three only mean something while a purge is requested, so each carries a CHECK
-- tying it to purge_requested_at.
--
-- sessions.mfa_verified_at already exists (0001_initial), as epoch ms: the fresh-MFA gate of
-- ADR 0019 needs no schema change, so this file does not touch sessions.
--
-- ALTER TABLE ... ADD COLUMN with a nullable column and no default is enough; devices is
-- referenced by many tables, so it is NOT rebuilt (a DROP of a referenced table would cascade
-- under foreign_keys=ON). SQLite writes each added column into sqlite_master right after the
-- last column definition, before the table CHECKs; replica/schema.sql carries the same text so
-- the two schemas compare equal (DH20). No table is added; one index is,
-- because every foreign-key column leads an index (replica/schema_test.py).

ALTER TABLE devices ADD COLUMN purge_requested_at INTEGER CHECK (purge_requested_at IS NULL OR (retired_at IS NOT NULL AND purge_requested_at >= 1000000000000));
ALTER TABLE devices ADD COLUMN purge_requested_by TEXT REFERENCES admin_users(id) ON DELETE SET NULL CHECK (purge_requested_by IS NULL OR purge_requested_at IS NOT NULL);
ALTER TABLE devices ADD COLUMN purge_reason TEXT CHECK (purge_reason IS NULL OR purge_requested_at IS NOT NULL);
ALTER TABLE devices ADD COLUMN purge_last_error_class TEXT CHECK (purge_last_error_class IS NULL OR purge_requested_at IS NOT NULL);
ALTER TABLE devices ADD COLUMN purge_last_attempt_at INTEGER CHECK (purge_last_attempt_at IS NULL OR purge_requested_at IS NOT NULL);
CREATE INDEX idx_devices_purge_requested_by ON devices(purge_requested_by);
