-- 0006_m3t6_token_delete
--
-- Deleting an enrollment token hides it; it does not remove the row (ticket
-- M3-T6, ADR 0016).
--
--   enrollment_tokens.deleted_at: when an admin deleted the token. A deleted
--   token leaves the lists; the row stays, because devices.enrollment_token_id
--   points at it (each device's enrolled-via link) and the audit trail names it.
--   Only a token that can no longer enroll may be hidden: it is revoked, or it had
--   expired by then. The CHECK holds that rule below the store, so no code path
--   can hide a live credential from the operator who would have to revoke it.
--
-- ALTER TABLE ... ADD COLUMN with a nullable column is enough; enrollment_tokens is
-- referenced by devices, so it is NOT rebuilt. SQLite writes the added column into
-- sqlite_master right after the last column definition, before the table CHECKs;
-- replica/schema.sql carries the same text so the two schemas compare equal (DH20).
-- Table and index counts are unchanged.

ALTER TABLE enrollment_tokens ADD COLUMN deleted_at INTEGER CHECK (deleted_at IS NULL OR revoked_at IS NOT NULL OR deleted_at >= expires_at);
