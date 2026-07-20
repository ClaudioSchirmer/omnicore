-- ── omnicore_mongo_views: blue-green view rebuild slots ───────────────────────
-- Two nullable pointers for the online blue-green view rebuild. A rebuild builds
-- a fresh physical collection (the shadow) while the live one keeps serving, then
-- flips readers to it with a single pointer write.
--
--   active_collection — the physical Mongo collection currently serving reads.
--                       NULL ⇒ the bare <view_name> collection is active. Every
--                       pre-blue-green row is already in this state, so no backfill
--                       is needed; the first rebuild sets an explicit slot name.
--   shadow_collection — the slot being built during a rebuild. NON-NULL is the
--                       dual-apply signal (recompose/delete into BOTH slots) and
--                       names the build target; NULL between rebuilds.
ALTER TABLE omnicore_mongo_views ADD COLUMN active_collection TEXT;
ALTER TABLE omnicore_mongo_views ADD COLUMN shadow_collection TEXT;
