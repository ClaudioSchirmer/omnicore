-- ── omnicore_mongo_views: blue-green view rebuild slots ───────────────────────
-- Two nullable pointers for the online blue-green view rebuild (only exercised
-- when Mongo projection is on).
--   active_collection — the physical collection currently serving reads; NULL ⇒
--                       the bare <view_name> collection is active.
--   shadow_collection — the slot being built during a rebuild (NON-NULL is the
--                       dual-apply signal); NULL between rebuilds.
ALTER TABLE omnicore_mongo_views ADD COLUMN active_collection TEXT;
ALTER TABLE omnicore_mongo_views ADD COLUMN shadow_collection TEXT;
