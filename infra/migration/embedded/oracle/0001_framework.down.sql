-- Reverse of 0001_framework.up.sql (Oracle). DROP TABLE removes the table's
-- indexes and constraints; IF EXISTS is native on the 23ai floor.
DROP TABLE IF EXISTS omnicore_integration_processed CASCADE CONSTRAINTS;
DROP TABLE IF EXISTS omnicore_integration_failures CASCADE CONSTRAINTS;
DROP TABLE IF EXISTS integration_events CASCADE CONSTRAINTS;
DROP TABLE IF EXISTS audit_events CASCADE CONSTRAINTS;
DROP TABLE IF EXISTS omnicore_mongo_views CASCADE CONSTRAINTS;
DROP TABLE IF EXISTS outbox CASCADE CONSTRAINTS;
