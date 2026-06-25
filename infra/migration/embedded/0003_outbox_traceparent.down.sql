ALTER TABLE audit_events       DROP COLUMN IF EXISTS trace_id;
ALTER TABLE integration_events DROP COLUMN IF EXISTS traceparent;
ALTER TABLE outbox             DROP COLUMN IF EXISTS traceparent;
