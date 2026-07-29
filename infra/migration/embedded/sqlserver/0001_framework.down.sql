-- Reverse of 0001_framework.up.sql (SQL Server). DROP TABLE removes the
-- table's indexes and constraints.
DROP TABLE IF EXISTS omnicore_integration_processed;
DROP TABLE IF EXISTS omnicore_integration_failures;
DROP TABLE IF EXISTS integration_events;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS omnicore_mongo_views;
DROP TABLE IF EXISTS outbox;
