-- Framework migration 3: carry the trace context across the framework's async
-- and forensic surfaces so a distributed trace can be reconstructed end to end.
--
--   outbox.traceparent              W3C traceparent of the producing request.
--                                   Debezium's Outbox Event Router maps it to a
--                                   Kafka header; the SyncEngine links the async
--                                   view projection back to the producing trace.
--   integration_events.traceparent  Same, for the integration-events async path
--                                   (Kafka → Receiver). Complements the existing
--                                   correlation_id/causation_id (which give the
--                                   trace id but not a linkable span context).
--   audit_events.trace_id           Pivot column: the 32-char hex trace id of the
--                                   request that produced the audit row, so a
--                                   forensic row jumps straight to its trace in
--                                   the collector. Not a carrier — audit is read,
--                                   never re-consumed.
--
-- All nullable: existing rows and writes made with tracing disabled store NULL.
ALTER TABLE outbox             ADD COLUMN IF NOT EXISTS traceparent VARCHAR(64);
ALTER TABLE integration_events ADD COLUMN IF NOT EXISTS traceparent VARCHAR(64);
ALTER TABLE audit_events       ADD COLUMN IF NOT EXISTS trace_id    VARCHAR(32);
