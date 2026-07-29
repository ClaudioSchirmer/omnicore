-- ── omnicore_projection_failures ──────────────────────────────────────────────
-- See postgres/0003_projection_failures.up.sql for the full semantics. The
-- read-side's UNIFIED failure ledger, discriminated by `kind`: 'event' parks a
-- whole projection event WITH ITS PAYLOAD (replayed from the row, no broker);
-- 'ripple' records one failed embed-segment refresh — the pair (source,
-- dependent view) — with NO payload (the replay recomposes from the source's
-- CURRENT document). Mirrors live state — a newer failure for the same key
-- overwrites the older one. One retry driver (mongo.parkedRetry) serves both.
CREATE TABLE omnicore_projection_failures (
    id              BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard).',
    kind            VARCHAR(16)  CHARACTER SET ascii NOT NULL COMMENT 'event | ripple — which replay the retry driver runs.',
    consumer_group  VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Sync consumer group that owns the replay.',
    topic           VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'event: source topic. ripple: the SOURCE — an upstream topic or view:<name>. (ascii VARCHAR(255) covers Kafka''s 249-char topic limit and keeps the composite UNIQUE inside MySQL''s 3072-byte index limit.)',
    aggregate_type  VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'event: aggregate type header. ripple: the DEPENDENT view being refreshed.',
    event_type      VARCHAR(32)  CHARACTER SET ascii NULL COMMENT 'event: verb (INSERTED | UPDATED | ARCHIVED | UNARCHIVED | DELETED). ripple: NULL.',
    aggregate_id    VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'event: aggregate id. ripple: the SOURCE document id.',
    stage           VARCHAR(16)  CHARACTER SET ascii NULL COMMENT 'ripple: discover | compose | upsert | signal. event: NULL.',
    local_id        VARCHAR(255) CHARACTER SET ascii NULL COMMENT 'ripple: the embedding document last touched, when known. event: NULL.',
    traceparent     VARCHAR(255) CHARACTER SET ascii NULL COMMENT 'W3C trace context from the producing write; NULL when tracing was off.',
    payload         LONGTEXT     NULL COMMENT 'event: the outbox payload verbatim. ripple: NULL (replay reads the current source document).',
    error           TEXT         NOT NULL COMMENT 'Last error message (overwritten per attempt).',
    attempt         INTEGER      NOT NULL DEFAULT 1 COMMENT 'Failure counter; auto-incremented when the same key fails again.',
    first_seen_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT 'When the key first parked (frozen).',
    last_attempt_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT 'When the last park/replay ran (refreshed).',
    resolved_at     DATETIME(6)  NULL COMMENT 'Set when a replay succeeds; NULL while pending.',

    PRIMARY KEY (id),
    KEY omnicore_projection_failures_pending_idx (consumer_group, last_attempt_at),
    CONSTRAINT omnicore_projection_failures_kind_valid CHECK (kind IN ('event', 'ripple')),
    CONSTRAINT omnicore_projection_failures_stage_valid CHECK (stage IS NULL OR stage IN ('discover', 'compose', 'upsert', 'signal')),
    CONSTRAINT omnicore_projection_failures_event_has_payload CHECK (kind <> 'event' OR payload IS NOT NULL),
    CONSTRAINT omnicore_projection_failures_attempt_positive CHECK (attempt > 0),
    CONSTRAINT omnicore_projection_failures_natural_key
        UNIQUE (consumer_group, kind, topic, aggregate_type, aggregate_id)
) COMMENT='Read-side unified failure ledger (mirrors live state); kind=event payload replay, kind=ripple recompose replay; replayed by the projection retry driver.';
