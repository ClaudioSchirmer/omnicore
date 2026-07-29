-- ── omnicore_projection_failures ──────────────────────────────────────────────
-- See postgres/0003_projection_failures.up.sql for the full semantics. The
-- read-side's UNIFIED failure ledger, discriminated by `kind`: 'event' parks a
-- whole projection event WITH ITS PAYLOAD (replayed from the row, no broker);
-- 'ripple' records one failed embed-segment refresh — the pair (source,
-- dependent view) — with NO payload (the replay recomposes from the source's
-- CURRENT document). Mirrors live state — a newer failure for the same key
-- overwrites the older one. One retry driver (mongo.parkedRetry) serves both.
CREATE TABLE omnicore_projection_failures (
    id              BINARY(16)    NOT NULL,
    kind            VARCHAR(16)   NOT NULL,
    consumer_group  VARCHAR(255)  NOT NULL,
    topic           VARCHAR(255)  NOT NULL,
    aggregate_type  VARCHAR(255)  NOT NULL,
    event_type      VARCHAR(32)   NULL,
    aggregate_id    VARCHAR(255)  NOT NULL,
    stage           VARCHAR(16)   NULL,
    local_id        VARCHAR(255)  NULL,
    traceparent     VARCHAR(255)  NULL,
    payload         NVARCHAR(MAX) NULL,
    error           NVARCHAR(MAX) NOT NULL,
    attempt         INTEGER       NOT NULL CONSTRAINT omnicore_projection_failures_attempt_default DEFAULT 1,
    first_seen_at   DATETIME2(6)  NOT NULL CONSTRAINT omnicore_projection_failures_first_seen_default DEFAULT CURRENT_TIMESTAMP,
    last_attempt_at DATETIME2(6)  NOT NULL CONSTRAINT omnicore_projection_failures_last_attempt_default DEFAULT CURRENT_TIMESTAMP,
    resolved_at     DATETIME2(6)  NULL,
    CONSTRAINT omnicore_projection_failures_pkey PRIMARY KEY CLUSTERED (id),
    CONSTRAINT omnicore_projection_failures_kind_valid CHECK (kind IN ('event', 'ripple')),
    CONSTRAINT omnicore_projection_failures_stage_valid CHECK (stage IS NULL OR stage IN ('discover', 'compose', 'upsert', 'signal')),
    CONSTRAINT omnicore_projection_failures_event_has_payload CHECK (kind <> 'event' OR payload IS NOT NULL),
    CONSTRAINT omnicore_projection_failures_attempt_positive CHECK (attempt > 0),
    CONSTRAINT omnicore_projection_failures_natural_key
        UNIQUE (consumer_group, kind, topic, aggregate_type, aggregate_id)
);
CREATE INDEX omnicore_projection_failures_pending_idx
    ON omnicore_projection_failures (consumer_group, last_attempt_at);
