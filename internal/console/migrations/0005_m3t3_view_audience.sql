-- 0005_m3t3_view_audience
--
-- Saved views carry an explicit audience (ticket M3-T3, architecture section
-- 5.2 /views).
--
--   saved_views.audience: who besides the owner sees a shared view. 'all' is
--   every customer: only accounts that see every customer see the view. A JSON
--   array of customer ids is those customers: an account sees the view only when
--   every id is inside its scope. NULL is no audience, for a private view; a
--   shared view with no readable audience is visible to its owner only (the store
--   fails closed). Who sees a view is never inferred from its name or its filter,
--   whose free text cannot be resolved to a customer.
--
-- Views that were shared before this migration get 'all': they disappear from
-- customer-scoped accounts until their owner names an audience. That is the safe
-- default; it hides, it never widens.
--
-- ALTER TABLE ... ADD COLUMN with a nullable column is enough; saved_views is
-- referenced by reports, so it is NOT rebuilt. SQLite writes the added column
-- into sqlite_master right after the last column definition, before the table
-- UNIQUE; replica/schema.sql carries the same text so the two schemas compare
-- equal (DH20). Table and index counts are unchanged.

ALTER TABLE saved_views ADD COLUMN audience TEXT CHECK (audience IS NULL OR audience = 'all' OR (json_valid(audience) AND json_type(audience) = 'array'));

UPDATE saved_views SET audience = 'all' WHERE shared = 1;
