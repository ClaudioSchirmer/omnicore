-- ╔══════════════════════════════════════════════════════════════════════════╗
-- ║ OmniCore framework control plane — PostgreSQL                              ║
-- ╠══════════════════════════════════════════════════════════════════════════╣
-- ║ Every table the framework owns, created in one migration. Service domain   ║
-- ║ tables start at version 0001 in the service's own directory. Each table +  ║
-- ║ column carries a COMMENT describing what it is for — the schema documents  ║
-- ║ itself (\d+ <table> in psql shows them).                                   ║
-- ╚══════════════════════════════════════════════════════════════════════════╝

-- ── outbox ──────────────────────────────────────────────────────────────────
-- The transactional outbox: one row per aggregate write, inserted in the SAME
-- transaction as the data row so the event can never be lost or double-emitted.
-- Debezium tails this table via logical replication and its Outbox Event Router
-- turns each row into a Kafka message (key=aggregate_id, headers carry the type).
CREATE TABLE outbox (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  VARCHAR(100) NOT NULL,
    event_type      VARCHAR(50)  NOT NULL,
    aggregate_id    UUID         NOT NULL,
    payload         JSONB        NOT NULL,
    traceparent     VARCHAR(64),
    created_at      TIMESTAMP    NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  outbox                IS 'Transactional outbox: one row per aggregate write, in-TX with the data row; tailed by Debezium and routed to Kafka.';
COMMENT ON COLUMN outbox.id             IS 'Surrogate row id (DB-generated). Not the business key — see aggregate_id.';
COMMENT ON COLUMN outbox.aggregate_type IS 'Logical aggregate/table name (e.g. "users"); Debezium routes it to topic "<aggregate_type>.events".';
COMMENT ON COLUMN outbox.event_type     IS 'Lifecycle verb: INSERTED | UPDATED | ARCHIVED | UNARCHIVED | DELETED. Carried as a Kafka header.';
COMMENT ON COLUMN outbox.aggregate_id   IS 'Id of the written aggregate root; becomes the Kafka message key (per-aggregate ordering).';
COMMENT ON COLUMN outbox.payload        IS 'JSON snapshot of the write (root fields + active children). Informational — the SyncEngine re-reads from Postgres.';
COMMENT ON COLUMN outbox.traceparent    IS 'W3C traceparent of the producing request; mapped to a Kafka header so the async projection links back to the trace. NULL when tracing is off.';
COMMENT ON COLUMN outbox.created_at      IS 'Insert timestamp (NOW()). Drives the created_at index used for pruning/replay.';

CREATE INDEX outbox_aggregate_idx  ON outbox (aggregate_type, aggregate_id);
CREATE INDEX outbox_created_at_idx ON outbox (created_at);

-- ── omnicore_mongo_views ──────────────────────────────────────────────────────
-- The Mongo read-side control plane: one row per managed view, recording its
-- declared shape (hashes) and rebuild state so boot can detect drift and a
-- crash mid-rebuild can be recovered. The advisory lock (pg_advisory_lock)
-- guards the rebuild; this table is the durable, forensic side of that.
CREATE TABLE omnicore_mongo_views (
    view_name              TEXT PRIMARY KEY,
    version                INTEGER     NOT NULL,
    rebuild_hash           VARCHAR(64) NOT NULL,
    artifact_hash          VARCHAR(64) NOT NULL,
    combined_hash          VARCHAR(64) NOT NULL,

    previous_version       INTEGER,
    previous_combined_hash VARCHAR(64),
    previous_applied_at    TIMESTAMP,

    status                 TEXT        NOT NULL DEFAULT 'done',
    started_at             TIMESTAMP,
    pid                    TEXT,
    host                   TEXT,

    applied_at             TIMESTAMP   NOT NULL,
    applied_by             TEXT        NOT NULL,
    code_version           TEXT,

    CONSTRAINT omnicore_mongo_views_version_positive CHECK (version > 0),
    CONSTRAINT omnicore_mongo_views_status_valid     CHECK (status IN ('done', 'processing'))
);

COMMENT ON TABLE  omnicore_mongo_views                        IS 'Mongo read-side registry: declared shape + rebuild state per view (drift detection + crash recovery).';
COMMENT ON COLUMN omnicore_mongo_views.view_name             IS 'Mongo collection / view name (primary key).';
COMMENT ON COLUMN omnicore_mongo_views.version               IS 'Developer-declared ViewDefinition.Version(N); bumped on any rebuild-relevant shape change.';
COMMENT ON COLUMN omnicore_mongo_views.rebuild_hash          IS 'SHA-256 over rebuild-relevant state (root, embeds, DeleteOnArchive, $jsonSchema, collation, capped, time-series). A change here means the docs must be recomposed.';
COMMENT ON COLUMN omnicore_mongo_views.artifact_hash         IS 'SHA-256 over indexes only — applied via ApplyMongoSpecs, no doc rewrite.';
COMMENT ON COLUMN omnicore_mongo_views.combined_hash         IS 'SHA-256 identity stamped on the row (rebuild_hash + artifact_hash); the value drift detection compares.';
COMMENT ON COLUMN omnicore_mongo_views.previous_version      IS 'Version before the last rebuild (rollback/forensics).';
COMMENT ON COLUMN omnicore_mongo_views.previous_combined_hash IS 'combined_hash before the last rebuild.';
COMMENT ON COLUMN omnicore_mongo_views.previous_applied_at   IS 'applied_at before the last rebuild.';
COMMENT ON COLUMN omnicore_mongo_views.status                IS 'Rebuild state machine: done | processing.';
COMMENT ON COLUMN omnicore_mongo_views.started_at            IS 'When the in-flight rebuild began; NULL when status=done.';
COMMENT ON COLUMN omnicore_mongo_views.pid                   IS 'OS pid holding the rebuild (forensics for a crashed/stuck rebuild).';
COMMENT ON COLUMN omnicore_mongo_views.host                  IS 'Host holding the rebuild (forensics).';
COMMENT ON COLUMN omnicore_mongo_views.applied_at            IS 'When the current shape was last applied.';
COMMENT ON COLUMN omnicore_mongo_views.applied_by            IS 'Who applied it: "<svc>@pid:<n>" or "manual-reconcile-*".';
COMMENT ON COLUMN omnicore_mongo_views.code_version          IS 'OMNICORE_CODE_VERSION env at apply time (build provenance); never a boot blocker.';

CREATE INDEX omnicore_mongo_views_applied_at_idx
    ON omnicore_mongo_views (applied_at DESC);

CREATE INDEX omnicore_mongo_views_status_idx
    ON omnicore_mongo_views (status)
    WHERE status <> 'done';

-- ── omnicore_upstream_failures ────────────────────────────────────────────────
-- Cross-service composition failure registry. When B materializes A's events
-- into local Mongo and a recompose fails, the failure is recorded here (mirrors
-- live state, not a growing log) so an operator can see "what is stuck" and the
-- framework can retry. The Kafka offset still advances (no poison pill).
CREATE TABLE omnicore_upstream_failures (
    id                  BIGSERIAL PRIMARY KEY,
    subscription_topic  TEXT      NOT NULL,
    view_name           TEXT      NOT NULL,
    upstream_id         TEXT      NOT NULL,
    local_id            TEXT      NOT NULL DEFAULT '',
    stage               TEXT      NOT NULL,
    error               TEXT      NOT NULL,
    attempt             INTEGER   NOT NULL DEFAULT 1,
    first_seen_at       TIMESTAMP NOT NULL DEFAULT NOW(),
    last_attempt_at     TIMESTAMP NOT NULL DEFAULT NOW(),
    resolved_at         TIMESTAMP,

    CONSTRAINT omnicore_upstream_failures_stage_valid
        CHECK (stage IN ('discover', 'compose', 'upsert')),
    CONSTRAINT omnicore_upstream_failures_attempt_positive
        CHECK (attempt > 0),
    CONSTRAINT omnicore_upstream_failures_natural_key
        UNIQUE (subscription_topic, view_name, upstream_id, local_id, stage)
);

COMMENT ON TABLE  omnicore_upstream_failures                    IS 'Cross-service recompose failure registry (mirrors live state). One row per (topic, view, upstream_id, local_id, stage); retried by UpstreamSubscriber.RetryPendingFailures.';
COMMENT ON COLUMN omnicore_upstream_failures.id                IS 'Surrogate row id.';
COMMENT ON COLUMN omnicore_upstream_failures.subscription_topic IS 'Upstream Kafka topic the subscription consumes.';
COMMENT ON COLUMN omnicore_upstream_failures.view_name         IS 'Local B view whose recompose failed.';
COMMENT ON COLUMN omnicore_upstream_failures.upstream_id       IS 'Id of the changed upstream (A) document that triggered the recompose.';
COMMENT ON COLUMN omnicore_upstream_failures.local_id          IS 'Id of the local B doc being recomposed; empty string on the "discover" stage (before a local doc is known).';
COMMENT ON COLUMN omnicore_upstream_failures.stage             IS 'Where it failed: discover | compose | upsert.';
COMMENT ON COLUMN omnicore_upstream_failures.error             IS 'Last error message (overwritten per retry).';
COMMENT ON COLUMN omnicore_upstream_failures.attempt           IS 'Retry counter; auto-incremented on conflict.';
COMMENT ON COLUMN omnicore_upstream_failures.first_seen_at     IS 'When the failure was first recorded (frozen).';
COMMENT ON COLUMN omnicore_upstream_failures.last_attempt_at   IS 'When the last retry ran (refreshed).';
COMMENT ON COLUMN omnicore_upstream_failures.resolved_at       IS 'Set when a clean recompose pass succeeds; NULL while pending.';

CREATE INDEX omnicore_upstream_failures_pending_idx
    ON omnicore_upstream_failures (subscription_topic, view_name, upstream_id)
    WHERE resolved_at IS NULL;

CREATE INDEX omnicore_upstream_failures_last_attempt_idx
    ON omnicore_upstream_failures (last_attempt_at DESC)
    WHERE resolved_at IS NULL;

-- ── audit_events ──────────────────────────────────────────────────────────────
-- The authoritative audit trail: one row per write, inserted in the SAME TX as
-- the data row when the "database" destination is on. Partitioned by created_at
-- (RANGE) so old partitions can be detached/dropped cheaply.
CREATE TABLE audit_events (
    id            UUID         NOT NULL DEFAULT gen_random_uuid(),
    aggregate_id  UUID         NOT NULL,
    entity_type   VARCHAR(255) NOT NULL,
    verb          VARCHAR(32)  NOT NULL,
    action_name   VARCHAR(64)  NOT NULL,
    kind          VARCHAR(16)  NOT NULL,
    actor         VARCHAR(255),
    actor_issuer  VARCHAR(255),
    tenant_id     VARCHAR(255),
    thread_id     UUID         NOT NULL,
    trace_id      VARCHAR(32),
    occurred_at   TIMESTAMP    NOT NULL,
    created_at    TIMESTAMP    NOT NULL DEFAULT NOW(),
    payload       JSONB        NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;

COMMENT ON TABLE  audit_events              IS 'Authoritative audit trail: one row per write, in-TX with the data row. Partitioned by created_at (RANGE) for cheap retention management.';
COMMENT ON COLUMN audit_events.id           IS 'Audit row id (DB-generated). PK is (id, created_at) — created_at is required in the key because the table is partitioned by it.';
COMMENT ON COLUMN audit_events.aggregate_id IS 'Id of the audited aggregate root.';
COMMENT ON COLUMN audit_events.entity_type  IS 'Entity/aggregate Go type name (e.g. "User").';
COMMENT ON COLUMN audit_events.verb         IS 'SQL-grounded verb: insert | update | archive | unarchive | delete (PUT and PATCH both map to update).';
COMMENT ON COLUMN audit_events.action_name  IS 'The Get* action that produced it (carries the PUT-vs-PATCH intent the verb collapses).';
COMMENT ON COLUMN audit_events.kind         IS 'Body regime: snapshot (insert/delete) | delta (update) | transition (archive/unarchive).';
COMMENT ON COLUMN audit_events.actor        IS 'Authenticated principal (JWT sub), or "anonymous". NULL only for writes made with no actor context.';
COMMENT ON COLUMN audit_events.actor_issuer IS 'JWT issuer (iss); NULL when no token.';
COMMENT ON COLUMN audit_events.tenant_id    IS 'Tenant claim when multi-tenant; NULL otherwise.';
COMMENT ON COLUMN audit_events.thread_id    IS 'Request id (AppContext.ID) tying the audit row to logs/traces of the same request.';
COMMENT ON COLUMN audit_events.trace_id     IS 'Pivot column: 32-char hex trace id of the producing request, to jump straight to the trace. NULL when tracing is off.';
COMMENT ON COLUMN audit_events.occurred_at  IS 'When the domain operation happened (business time).';
COMMENT ON COLUMN audit_events.created_at   IS 'When the row was written (NOW()); the partition key.';
COMMENT ON COLUMN audit_events.payload      IS 'JSON body: snapshot | changes (delta) | children, per kind.';

CREATE INDEX audit_events_entity_timeline_idx
    ON audit_events (entity_type, aggregate_id, occurred_at DESC);
CREATE INDEX audit_events_actor_idx
    ON audit_events (actor, occurred_at DESC) WHERE actor IS NOT NULL;
CREATE INDEX audit_events_tenant_idx
    ON audit_events (tenant_id, occurred_at DESC) WHERE tenant_id IS NOT NULL;
CREATE INDEX audit_events_thread_idx
    ON audit_events (thread_id);
CREATE INDEX audit_events_created_at_brin
    ON audit_events USING BRIN (created_at);

-- ── integration_events ────────────────────────────────────────────────────────
-- Producer-side authoritative store of cross-service integration events. Written
-- in the data write's TX when fwintegration.Dispatch is called WithTx(tx).
-- aggregate_type/aggregate_id are NULLABLE so a standalone event (no aggregate
-- binding) can dispatch; actor is NOT NULL (ctx.ActorSubject() returns the
-- "anonymous" sentinel when no JWT is attached).
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
    traceparent     VARCHAR(64),
    actor           VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP    NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE  integration_events                IS 'Producer-side authoritative store of cross-service integration events (in-TX with the data row under WithTx). Forensic timeline + replay source.';
COMMENT ON COLUMN integration_events.id             IS 'Surrogate row id.';
COMMENT ON COLUMN integration_events.event_id       IS 'Globally-unique event id (UUID); the consumer-side dedup key. UNIQUE.';
COMMENT ON COLUMN integration_events.aggregate_type IS 'Aggregate type the event is about; NULL for a standalone event.';
COMMENT ON COLUMN integration_events.aggregate_id   IS 'Aggregate id the event is about; NULL for a standalone event.';
COMMENT ON COLUMN integration_events.event_type     IS 'Wire event type header value (e.g. "UserActivated").';
COMMENT ON COLUMN integration_events.event_version  IS 'Schema version of the event payload (sibling-key migration of breaking changes).';
COMMENT ON COLUMN integration_events.payload        IS 'JSON event payload (the cross-service contract).';
COMMENT ON COLUMN integration_events.correlation_id IS 'Correlation id of the originating request chain (== trace id); NULL if none.';
COMMENT ON COLUMN integration_events.causation_id   IS 'Id of the event/command that caused this one; NULL if none.';
COMMENT ON COLUMN integration_events.thread_id      IS 'Producing request id (AppContext.ID).';
COMMENT ON COLUMN integration_events.traceparent    IS 'W3C traceparent of the producing request (links the async consumer span to the producer trace). NULL when tracing is off.';
COMMENT ON COLUMN integration_events.actor          IS 'Producing principal (JWT sub) or "anonymous" sentinel; never empty.';
COMMENT ON COLUMN integration_events.created_at     IS 'Insert timestamp (NOW()).';

CREATE INDEX integration_events_aggregate_idx
    ON integration_events (aggregate_type, aggregate_id, created_at)
    WHERE aggregate_type IS NOT NULL;
CREATE INDEX integration_events_event_type_idx
    ON integration_events (event_type, created_at);

-- ── omnicore_integration_failures ─────────────────────────────────────────────
-- Consumer-side integration failure registry (mirrors omnicore_upstream_failures).
-- One row per (consumer_group, source_key, event_key, event_id), upserted on each
-- retry, so an operator can query "which events are stuck right now?".
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

COMMENT ON TABLE  omnicore_integration_failures                IS 'Consumer-side integration handler failure registry; retried by Receiver.RetryPendingFailures.';
COMMENT ON COLUMN omnicore_integration_failures.id            IS 'Surrogate row id.';
COMMENT ON COLUMN omnicore_integration_failures.consumer_group IS 'Kafka consumer group that failed to process the event.';
COMMENT ON COLUMN omnicore_integration_failures.source_key    IS 'Go-side source identifier (the "From(...)" key), stable across renames of the wire topic.';
COMMENT ON COLUMN omnicore_integration_failures.event_key     IS 'Go-side event identifier (the "On(...)" key).';
COMMENT ON COLUMN omnicore_integration_failures.event_id      IS 'The failing event''s id (UUID).';
COMMENT ON COLUMN omnicore_integration_failures.payload       IS 'JSON payload of the failing event (for retry/inspection).';
COMMENT ON COLUMN omnicore_integration_failures.error         IS 'Last error message (overwritten per retry).';
COMMENT ON COLUMN omnicore_integration_failures.attempt       IS 'Retry counter; auto-incremented on conflict.';
COMMENT ON COLUMN omnicore_integration_failures.first_seen_at IS 'When first recorded (frozen).';
COMMENT ON COLUMN omnicore_integration_failures.last_attempt_at IS 'When last retried (refreshed).';
COMMENT ON COLUMN omnicore_integration_failures.resolved_at   IS 'Set when the event is finally processed; NULL while pending.';

CREATE INDEX omnicore_integration_failures_pending_idx
    ON omnicore_integration_failures (consumer_group, source_key, event_key)
    WHERE resolved_at IS NULL;

CREATE INDEX omnicore_integration_failures_last_attempt_idx
    ON omnicore_integration_failures (last_attempt_at DESC)
    WHERE resolved_at IS NULL;

-- ── omnicore_integration_processed ────────────────────────────────────────────
-- Consumer-side dedup table. Pre-checked on every Kafka message: a hit means
-- "already processed by this consumer group" → skip the handler. Composite PK
-- (event_id, consumer_group) lets N groups dedup the same event independently.
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

COMMENT ON TABLE  omnicore_integration_processed                IS 'Consumer-side dedup: one row per (event_id, consumer_group) successfully processed. Pre-checked before each handler run; at-least-once delivery means handlers must still be idempotent.';
COMMENT ON COLUMN omnicore_integration_processed.event_id      IS 'Processed event id (part of PK).';
COMMENT ON COLUMN omnicore_integration_processed.consumer_group IS 'Consumer group that processed it (part of PK; lets groups dedup independently).';
COMMENT ON COLUMN omnicore_integration_processed.source_key    IS 'Go-side source identifier (diagnostics).';
COMMENT ON COLUMN omnicore_integration_processed.event_key     IS 'Go-side event identifier (diagnostics).';
COMMENT ON COLUMN omnicore_integration_processed.topic         IS 'Kafka topic the event came from (diagnostics).';
COMMENT ON COLUMN omnicore_integration_processed.event_type    IS 'Wire event type (diagnostics).';
COMMENT ON COLUMN omnicore_integration_processed.processed_at  IS 'When processed (NOW()); BRIN-indexed for cheap pruning (DELETE WHERE processed_at < NOW() - INTERVAL ...).';

CREATE INDEX omnicore_integration_processed_processed_at_brin
    ON omnicore_integration_processed USING BRIN (processed_at);
