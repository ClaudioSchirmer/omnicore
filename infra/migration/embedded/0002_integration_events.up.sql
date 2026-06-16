-- Integration events — the producer-side authoritative store. Written in
-- the same pgx.Tx as the data row + outbox + audit by fwintegration.Dispatch
-- when invoked with WithTx(tx) from a BeforeCommit hook. Schema fields
-- mirror the per-event metadata the framework auto-fills + YAML resolves
-- + dev provides at call time. aggregate_type / aggregate_id are NULLABLE
-- so standalone events (no aggregate binding) can dispatch without a fake
-- identity; actor is NOT NULL because ctx.ActorSubject() is contractually
-- non-empty (returns "anonymous" sentinel when no JWT is attached).
CREATE TABLE integration_events (
    id              BIGSERIAL    PRIMARY KEY,
    event_id        UUID         NOT NULL UNIQUE,
    aggregate_type  VARCHAR(100),
    aggregate_id    UUID,
    event_type      VARCHAR(100) NOT NULL,
    event_version   INTEGER      NOT NULL DEFAULT 1,
    payload         JSONB        NOT NULL,
    correlation_id  UUID,
    causation_id    UUID,
    thread_id       UUID         NOT NULL,
    actor           VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP    NOT NULL DEFAULT NOW()
);

-- Forensic timeline index: scoped to events bound to an aggregate so
-- standalone events stay out of an aggregate-keyed index. Operator query:
--   SELECT ... FROM integration_events
--    WHERE aggregate_type = 'User' AND aggregate_id = $1
--    ORDER BY created_at;
CREATE INDEX integration_events_aggregate_idx
    ON integration_events (aggregate_type, aggregate_id, created_at)
    WHERE aggregate_type IS NOT NULL;

-- "events of type X over a time window" lookups (replay tooling, audit
-- reports, consumer-onboarding bootstrap).
CREATE INDEX integration_events_event_type_idx
    ON integration_events (event_type, created_at);

-- omnicore_integration_failures mirrors omnicore_upstream_failures: one
-- row per (consumer_group, source_key, event_key, event_id), upserted on
-- each retry. Persisted alongside the slog warn line so an operator can
-- query "which events are stuck right now?" via plain SQL.
CREATE TABLE omnicore_integration_failures (
    id              BIGSERIAL PRIMARY KEY,
    consumer_group  TEXT      NOT NULL,
    source_key      TEXT      NOT NULL,
    event_key       TEXT      NOT NULL,
    event_id        UUID      NOT NULL,
    payload         JSONB     NOT NULL,
    error           TEXT      NOT NULL,
    attempt         INTEGER   NOT NULL DEFAULT 1,
    first_seen_at   TIMESTAMP NOT NULL DEFAULT NOW(),
    last_attempt_at TIMESTAMP NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMP,

    CONSTRAINT omnicore_integration_failures_attempt_positive
        CHECK (attempt > 0),
    CONSTRAINT omnicore_integration_failures_natural_key
        UNIQUE (consumer_group, source_key, event_key, event_id)
);

CREATE INDEX omnicore_integration_failures_pending_idx
    ON omnicore_integration_failures (consumer_group, source_key, event_key)
    WHERE resolved_at IS NULL;

CREATE INDEX omnicore_integration_failures_last_attempt_idx
    ON omnicore_integration_failures (last_attempt_at DESC)
    WHERE resolved_at IS NULL;

-- omnicore_integration_processed is the consumer-side dedup table. Pre-
-- check on every Kafka message: a hit means "already processed by this
-- consumer group" and the framework skips the handler. Composite PK
-- (event_id, consumer_group) lets N consumer groups dedup the same event
-- independently. BRIN on processed_at supports efficient operator-driven
-- pruning via DELETE WHERE processed_at < NOW() - INTERVAL '30 days'.
CREATE TABLE omnicore_integration_processed (
    event_id        UUID      NOT NULL,
    consumer_group  TEXT      NOT NULL,
    source_key      TEXT      NOT NULL,
    event_key       TEXT      NOT NULL,
    topic           TEXT      NOT NULL,
    event_type      TEXT      NOT NULL,
    processed_at    TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (event_id, consumer_group)
);

CREATE INDEX omnicore_integration_processed_processed_at_brin
    ON omnicore_integration_processed USING BRIN (processed_at);
