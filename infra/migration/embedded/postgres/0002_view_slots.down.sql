-- Reverse of 0002_view_slots.up.sql. The bare <view_name> collection is what an
-- older (pre-blue-green) binary reads; if a rebuild had already flipped to a slot
-- and dropped the bare collection, that binary rebuilds it from source via its
-- existing DriftMongoWiped path. This down migration only drops the columns.
ALTER TABLE omnicore_mongo_views DROP COLUMN IF EXISTS shadow_collection;
ALTER TABLE omnicore_mongo_views DROP COLUMN IF EXISTS active_collection;
