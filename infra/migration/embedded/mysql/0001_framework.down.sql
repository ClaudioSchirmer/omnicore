-- Reverse of 0001_framework.up.sql (MySQL). DROP TABLE removes the table's keys.
DROP TABLE IF EXISTS omnicore_integration_processed;
DROP TABLE IF EXISTS omnicore_integration_failures;
DROP TABLE IF EXISTS integration_events;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS omnicore_mongo_views;
DROP TABLE IF EXISTS outbox;
