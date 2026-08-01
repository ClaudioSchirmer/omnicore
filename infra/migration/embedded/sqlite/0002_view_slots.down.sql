-- Reverse of 0002_view_slots.up.sql. SQLite (3.35+) supports ALTER TABLE DROP
-- COLUMN, but with no IF EXISTS clause (unlike Postgres); the columns are known
-- to exist here since this only runs to reverse the up migration.
ALTER TABLE omnicore_mongo_views DROP COLUMN shadow_collection;
ALTER TABLE omnicore_mongo_views DROP COLUMN active_collection;
