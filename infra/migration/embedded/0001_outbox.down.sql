DROP INDEX IF EXISTS audit_events_created_at_brin;
DROP INDEX IF EXISTS audit_events_thread_idx;
DROP INDEX IF EXISTS audit_events_tenant_idx;
DROP INDEX IF EXISTS audit_events_actor_idx;
DROP INDEX IF EXISTS audit_events_entity_timeline_idx;
DROP TABLE IF EXISTS audit_events;

DROP INDEX IF EXISTS omnicore_upstream_failures_last_attempt_idx;
DROP INDEX IF EXISTS omnicore_upstream_failures_pending_idx;
DROP TABLE IF EXISTS omnicore_upstream_failures;

DROP INDEX IF EXISTS omnicore_mongo_views_status_idx;
DROP INDEX IF EXISTS omnicore_mongo_views_applied_at_idx;
DROP TABLE IF EXISTS omnicore_mongo_views;

DROP INDEX IF EXISTS outbox_created_at_idx;
DROP INDEX IF EXISTS outbox_aggregate_idx;
DROP TABLE IF EXISTS outbox;
