-- 0004_m2w1a
--
-- TOTP replay guard (ticket M2-W1A, architecture section 5.2 auth/mfa, feature row r98).
--
--   admin_users.totp_last_step: the RFC 6238 time step (unix seconds / 30) of
--   the last TOTP code the account used, at login, at a fresh-MFA check or at
--   enrollment. A code is accepted only for a step strictly greater than it, so
--   the same code, or an older one still inside the +/-1 step window, cannot be
--   used twice. The check and the update are one UPDATE ... WHERE
--   totp_last_step < ?, so two concurrent uses of one code cannot both win.
--   NULL means no code has been used since the column appeared (or since a
--   break-glass reset-mfa cleared the enrollment).
--
-- ALTER TABLE ... ADD COLUMN is enough (a nullable column without a default);
-- admin_users is referenced by many tables, so it is NOT rebuilt. SQLite writes
-- the added column into sqlite_master right after the last column definition,
-- before the table CHECKs; replica/schema.sql carries the same text so the two
-- schemas compare equal (DH20). Table and index counts are unchanged.

ALTER TABLE admin_users ADD COLUMN totp_last_step INTEGER;
