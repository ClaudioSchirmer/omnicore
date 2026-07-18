-- ── omnicore_mongo_views: blue-green view rebuild slots ───────────────────────
-- See postgres/0002_view_slots.up.sql for the full semantics. active_collection
-- NULL ⇒ the bare <view_name> collection is active (pre-blue-green state, no
-- backfill needed); shadow_collection NON-NULL is the dual-apply signal + target.
ALTER TABLE omnicore_mongo_views ADD (
    active_collection VARCHAR2(255 CHAR),
    shadow_collection VARCHAR2(255 CHAR)
);
