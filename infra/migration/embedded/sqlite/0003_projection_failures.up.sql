-- ── omnicore_projection_failures ──────────────────────────────────────────────
-- The read-side's UNIFIED failure ledger (mirrors live state, not a growing log:
-- one row per natural key, newest failure overwrites). kind=event: a parked
-- projection event (payload replay). kind=ripple: a failed embed-segment refresh
-- (recompose replay). Only exercised when Mongo projection is on.
CREATE TABLE omnicore_projection_failures (
    id              TEXT    PRIMARY KEY,         -- surrogate row id (UUID v7, Go-minted)
    kind            TEXT    NOT NULL,            -- event | ripple
    consumer_group  TEXT    NOT NULL,
    topic           TEXT    NOT NULL,            -- event: source topic. ripple: source (upstream topic / view:<name>)
    aggregate_type  TEXT    NOT NULL,            -- event: aggregate type. ripple: dependent view
    event_type      TEXT,                        -- event: verb. ripple: NULL
    aggregate_id    TEXT    NOT NULL,            -- event: aggregate id. ripple: source document id
    stage           TEXT,                        -- ripple: discover | compose | upsert | signal. event: NULL
    local_id        TEXT,                        -- ripple: embedding document last touched. event: NULL
    traceparent     TEXT,                        -- W3C trace context; NULL when tracing off
    payload         TEXT,                        -- event: outbox payload verbatim. ripple: NULL
    error           TEXT    NOT NULL,            -- last error message
    attempt         INTEGER NOT NULL DEFAULT 1,
    first_seen_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
    last_attempt_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
    resolved_at     TEXT,

    CONSTRAINT omnicore_projection_failures_kind_valid
        CHECK (kind IN ('event', 'ripple')),
    CONSTRAINT omnicore_projection_failures_stage_valid
        CHECK (stage IS NULL OR stage IN ('discover', 'compose', 'upsert', 'signal')),
    CONSTRAINT omnicore_projection_failures_event_has_payload
        CHECK (kind <> 'event' OR payload IS NOT NULL),
    CONSTRAINT omnicore_projection_failures_attempt_positive
        CHECK (attempt > 0),
    CONSTRAINT omnicore_projection_failures_natural_key
        UNIQUE (consumer_group, kind, topic, aggregate_type, aggregate_id)
);
CREATE INDEX omnicore_projection_failures_pending_idx
    ON omnicore_projection_failures (consumer_group, last_attempt_at)
    WHERE resolved_at IS NULL;
