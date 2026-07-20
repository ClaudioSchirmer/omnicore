-- Reverse of 0002_view_slots.up.sql. Columns only; Mongo-side rollback is handled
-- by the older binary's existing DriftMongoWiped rebuild path.
ALTER TABLE omnicore_mongo_views
    DROP COLUMN shadow_collection,
    DROP COLUMN active_collection;
