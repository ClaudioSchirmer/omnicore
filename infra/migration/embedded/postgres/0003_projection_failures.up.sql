-- ── omnicore_projection_failures ──────────────────────────────────────────────
-- The read-side's UNIFIED failure ledger. Every piece of deferred read-side
-- work lands here, discriminated by `kind`:
--
--   kind = 'event'  — a whole projection EVENT whose in-process retry budget
--     was exhausted. The row carries the outbox payload, so the retry driver
--     replays it without the broker. Holding the event would stall every
--     healthy aggregate behind it on the same partition; dropping it would be
--     silent, permanent divergence — parking dissolves the choice.
--
--   kind = 'ripple' — one EMBED-SEGMENT refresh that failed: the pair
--     (source, dependent view) whose materialized copy could not be brought
--     up to date. The source is either another service's mirror (an upstream
--     subscription topic) or a local view (labelled 'view:<name>'). NO payload
--     is stored: a ripple replay must recompose from the source's CURRENT
--     document — replaying a stale copy is exactly what the revision guards
--     exist to prevent.
--
-- One retry driver serves both kinds (the SyncEngine's parked-retry loop, the
-- mongo.parkedRetry knob): events re-project from their stored payload,
-- ripples re-run the segment recompose for their pair.
--
-- It mirrors LIVE STATE, not a growing log: one row per natural key, and a
-- newer failure for the same key OVERWRITES the older one (latest payload /
-- stage / error win). That is correct because the projection is state-based —
-- the newest state supersedes completely; per-aggregate ordering (hash
-- dispatch to one worker) is what makes "newer" well defined.
CREATE TABLE omnicore_projection_failures (
    id              UUID      PRIMARY KEY,
    kind            TEXT      NOT NULL,
    consumer_group  TEXT      NOT NULL,
    topic           TEXT      NOT NULL,
    aggregate_type  TEXT      NOT NULL,
    event_type      TEXT,
    aggregate_id    TEXT      NOT NULL,
    stage           TEXT,
    local_id        TEXT,
    traceparent     TEXT,
    payload         TEXT,
    error           TEXT      NOT NULL,
    attempt         INTEGER   NOT NULL DEFAULT 1,
    first_seen_at   TIMESTAMP NOT NULL DEFAULT NOW(),
    last_attempt_at TIMESTAMP NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMP,

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
COMMENT ON TABLE  omnicore_projection_failures                IS 'Read-side unified failure ledger (mirrors live state). kind=event: parked projection event (payload replay). kind=ripple: failed embed-segment refresh (recompose replay). One row per (consumer_group, kind, topic, aggregate_type, aggregate_id); replayed by the projection retry driver.';
COMMENT ON COLUMN omnicore_projection_failures.id             IS 'Surrogate row id — UUID v7 minted in Go (the framework id standard).';
COMMENT ON COLUMN omnicore_projection_failures.kind           IS 'event | ripple — which replay the retry driver runs.';
COMMENT ON COLUMN omnicore_projection_failures.consumer_group IS 'Sync consumer group that owns the replay, so multi-consumer services do not interfere.';
COMMENT ON COLUMN omnicore_projection_failures.topic          IS 'event: source topic. ripple: the SOURCE — an upstream subscription topic, or view:<name> for a local view.';
COMMENT ON COLUMN omnicore_projection_failures.aggregate_type IS 'event: aggregate type header. ripple: the DEPENDENT view being refreshed.';
COMMENT ON COLUMN omnicore_projection_failures.event_type     IS 'event: verb (INSERTED | UPDATED | ARCHIVED | UNARCHIVED | DELETED). ripple: NULL.';
COMMENT ON COLUMN omnicore_projection_failures.aggregate_id   IS 'event: aggregate id. ripple: the SOURCE document id.';
COMMENT ON COLUMN omnicore_projection_failures.stage          IS 'ripple: where it failed — discover | compose | upsert | signal (the post-write source re-read). event: NULL.';
COMMENT ON COLUMN omnicore_projection_failures.local_id       IS 'ripple: the embedding document last touched, when known. event: NULL.';
COMMENT ON COLUMN omnicore_projection_failures.traceparent    IS 'W3C trace context from the producing write; NULL when tracing was off.';
COMMENT ON COLUMN omnicore_projection_failures.payload        IS 'event: the outbox payload verbatim — the state a replay projects from. ripple: NULL (replay reads the current source document).';
COMMENT ON COLUMN omnicore_projection_failures.error          IS 'Last error message (overwritten per attempt).';
COMMENT ON COLUMN omnicore_projection_failures.attempt        IS 'Failure counter; auto-incremented when the same key fails again.';
COMMENT ON COLUMN omnicore_projection_failures.first_seen_at  IS 'When the key first parked (frozen).';
COMMENT ON COLUMN omnicore_projection_failures.last_attempt_at IS 'When the last park/replay ran (refreshed).';
COMMENT ON COLUMN omnicore_projection_failures.resolved_at    IS 'Set when a replay succeeds; NULL while pending.';
CREATE INDEX omnicore_projection_failures_pending_idx
    ON omnicore_projection_failures (consumer_group, last_attempt_at)
    WHERE resolved_at IS NULL;
