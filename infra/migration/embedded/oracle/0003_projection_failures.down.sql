-- Reverse of 0003_projection_failures.up.sql. Dropping the ledger discards any
-- event still parked in it; a binary without this table falls back to the
-- reconciliation sweep for repair.
DROP TABLE omnicore_projection_failures;
