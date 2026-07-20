-- ╔══════════════════════════════════════════════════════════════════════════╗
-- ║ OmniCore framework control plane — SQL Server                              ║
-- ╠══════════════════════════════════════════════════════════════════════════╣
-- ║ The T-SQL flavor of the same logical schema as embedded/postgres and       ║
-- ║ embedded/mysql. Every PK is a UUID v7 minted in Go, stored BINARY(16)      ║
-- ║ (the framework id standard); wire-crossing id references stay CHAR(36),    ║
-- ║ JSONB/JSON→NVARCHAR(MAX), BIGSERIAL/AUTO_INCREMENT→BIGINT IDENTITY(1,1),   ║
-- ║ TIMESTAMP/DATETIME→DATETIME2(6) + DEFAULT CURRENT_TIMESTAMP, TEXT→         ║
-- ║ NVARCHAR(MAX). PG's partial indexes become filtered indexes (native here); ║
-- ║ BRIN becomes a plain B-tree; no RANGE partitioning (plain audit table,     ║
-- ║ like MySQL). PK/UNIQUE/CHECK constraint names are IDENTICAL across the     ║
-- ║ three dialects so a ConstraintBinding maps the same violation to the same  ║
-- ║ HTTP status on every backend. Column semantics are documented in the       ║
-- ║ sibling files (MySQL carries per-column COMMENTs; T-SQL has no inline      ║
-- ║ comment syntax and extended properties would triple the file).             ║
-- ╚══════════════════════════════════════════════════════════════════════════╝

-- ── outbox ──────────────────────────────────────────────────────────────────
-- One row per aggregate write, in-TX with the data row; tailed by Debezium
-- (CDC on this table) and routed to the broker.
CREATE TABLE outbox (
    id              BINARY(16)    NOT NULL,
    aggregate_type  VARCHAR(100)  NOT NULL,
    event_type      VARCHAR(50)   NOT NULL,
    aggregate_id    CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    payload         NVARCHAR(MAX) NOT NULL,
    traceparent     VARCHAR(64)   NULL,
    created_at      DATETIME2(6)  NOT NULL CONSTRAINT outbox_created_at_default DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT outbox_pkey PRIMARY KEY CLUSTERED (id)
);
-- No (aggregate_type, aggregate_id) index: no framework code SELECTs the
-- outbox by aggregate (Debezium reads CDC, not the table); created_at stays
-- for pruning.
CREATE INDEX outbox_created_at_idx ON outbox (created_at);

-- ── omnicore_mongo_views ──────────────────────────────────────────────────────
-- Mongo read-side registry: declared shape + rebuild state per view (drift
-- detection + crash recovery).
CREATE TABLE omnicore_mongo_views (
    id                     BINARY(16)    NOT NULL,
    view_name              NVARCHAR(255) NOT NULL,
    version                INTEGER       NOT NULL,
    rebuild_hash           VARCHAR(64)   NOT NULL,
    artifact_hash          VARCHAR(64)   NOT NULL,
    combined_hash          VARCHAR(64)   NOT NULL,
    previous_version       INTEGER       NULL,
    previous_combined_hash VARCHAR(64)   NULL,
    previous_applied_at    DATETIME2(6)  NULL,
    status                 VARCHAR(16)   NOT NULL CONSTRAINT omnicore_mongo_views_status_default DEFAULT 'done',
    started_at             DATETIME2(6)  NULL,
    pid                    VARCHAR(64)   NULL,
    host                   NVARCHAR(255) NULL,
    applied_at             DATETIME2(6)  NOT NULL,
    applied_by             NVARCHAR(255) NOT NULL,
    code_version           NVARCHAR(255) NULL,
    CONSTRAINT omnicore_mongo_views_pkey PRIMARY KEY CLUSTERED (id),
    CONSTRAINT omnicore_mongo_views_view_name_key UNIQUE (view_name),
    CONSTRAINT omnicore_mongo_views_version_positive CHECK (version > 0),
    CONSTRAINT omnicore_mongo_views_status_valid     CHECK (status IN ('done', 'processing'))
);
CREATE INDEX omnicore_mongo_views_applied_at_idx ON omnicore_mongo_views (applied_at);
CREATE INDEX omnicore_mongo_views_status_idx     ON omnicore_mongo_views (status);

-- ── omnicore_upstream_failures ────────────────────────────────────────────────
-- Cross-service recompose failure registry (mirrors live state); retried by
-- UpstreamSubscriber.RetryPendingFailures. Technical identifiers are VARCHAR
-- (single-byte, ASCII by contract), mirroring the MySQL ascii columns.
CREATE TABLE omnicore_upstream_failures (
    id                  BINARY(16)    NOT NULL,
    subscription_topic  VARCHAR(255)  NOT NULL,
    view_name           VARCHAR(255)  NOT NULL,
    upstream_id         VARCHAR(255)  NOT NULL,
    local_id            VARCHAR(255)  NOT NULL CONSTRAINT omnicore_upstream_failures_local_id_default DEFAULT '',
    stage               VARCHAR(16)   NOT NULL,
    error               NVARCHAR(MAX) NOT NULL,
    attempt             INTEGER       NOT NULL CONSTRAINT omnicore_upstream_failures_attempt_default DEFAULT 1,
    first_seen_at       DATETIME2(6)  NOT NULL CONSTRAINT omnicore_upstream_failures_first_seen_default DEFAULT CURRENT_TIMESTAMP,
    last_attempt_at     DATETIME2(6)  NOT NULL CONSTRAINT omnicore_upstream_failures_last_attempt_default DEFAULT CURRENT_TIMESTAMP,
    resolved_at         DATETIME2(6)  NULL,
    CONSTRAINT omnicore_upstream_failures_pkey PRIMARY KEY CLUSTERED (id),
    CONSTRAINT omnicore_upstream_failures_stage_valid      CHECK (stage IN ('discover', 'compose', 'upsert')),
    CONSTRAINT omnicore_upstream_failures_attempt_positive CHECK (attempt > 0),
    CONSTRAINT omnicore_upstream_failures_natural_key
        UNIQUE (subscription_topic, view_name, upstream_id, local_id, stage)
);
CREATE INDEX omnicore_upstream_failures_pending_idx
    ON omnicore_upstream_failures (subscription_topic, view_name, upstream_id);
CREATE INDEX omnicore_upstream_failures_last_attempt_idx
    ON omnicore_upstream_failures (last_attempt_at);

-- ── audit_events ──────────────────────────────────────────────────────────────
-- Authoritative audit trail: one row per write, in-TX with the data row. Plain
-- table (no RANGE partitioning, like MySQL); a B-tree on created_at replaces
-- the PG BRIN.
CREATE TABLE audit_events (
    id            BINARY(16)    NOT NULL,
    aggregate_id  CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    entity_type   NVARCHAR(255) NOT NULL,
    verb          VARCHAR(32)   NOT NULL,
    action_name   VARCHAR(64)   NOT NULL,
    kind          VARCHAR(16)   NOT NULL,
    actor         NVARCHAR(255) NULL,
    actor_issuer  NVARCHAR(255) NULL,
    tenant_id     NVARCHAR(255) NULL,
    thread_id     CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    trace_id      VARCHAR(32)   NULL,
    occurred_at   DATETIME2(6)  NOT NULL,
    created_at    DATETIME2(6)  NOT NULL CONSTRAINT audit_events_created_at_default DEFAULT CURRENT_TIMESTAMP,
    payload       NVARCHAR(MAX) NOT NULL,
    CONSTRAINT audit_events_pkey PRIMARY KEY CLUSTERED (id)
);
-- Only the entity-timeline index (serves FindByAggregate); FindByID is served by
-- the clustered PK. audit_events is written in every write's TX, so it carries the
-- minimum index set — forensic indexes (actor/tenant/thread/time) are ad-hoc,
-- added by devops when a deployment needs them.
CREATE INDEX audit_events_entity_timeline_idx ON audit_events (entity_type, aggregate_id, occurred_at DESC);

-- ── integration_events ────────────────────────────────────────────────────────
-- Producer-side authoritative store of cross-service integration events (in-TX
-- under WithTx). Forensic timeline + replay source.
CREATE TABLE integration_events (
    id              BINARY(16)    NOT NULL,
    event_id        CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    aggregate_type  VARCHAR(100)  NULL,
    aggregate_id    CHAR(36)      COLLATE Latin1_General_100_BIN2 NULL,
    event_type      VARCHAR(100)  NOT NULL,
    event_version   INTEGER       NOT NULL CONSTRAINT integration_events_event_version_default DEFAULT 1,
    payload         NVARCHAR(MAX) NOT NULL,
    correlation_id  CHAR(36)      COLLATE Latin1_General_100_BIN2 NULL,
    causation_id    CHAR(36)      COLLATE Latin1_General_100_BIN2 NULL,
    thread_id       CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    traceparent     VARCHAR(64)   NULL,
    actor           NVARCHAR(255) NOT NULL,
    created_at      DATETIME2(6)  NOT NULL CONSTRAINT integration_events_created_at_default DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT integration_events_pkey PRIMARY KEY CLUSTERED (id),
    CONSTRAINT integration_events_event_id_uniq UNIQUE (event_id)
);
CREATE INDEX integration_events_aggregate_idx  ON integration_events (aggregate_type, aggregate_id, created_at);
CREATE INDEX integration_events_event_type_idx ON integration_events (event_type, created_at);

-- ── omnicore_integration_failures ─────────────────────────────────────────────
-- Consumer-side integration handler failure registry; retried by
-- Receiver.RetryPendingFailures.
CREATE TABLE omnicore_integration_failures (
    id              BINARY(16)    NOT NULL,
    consumer_group  VARCHAR(255)  NOT NULL,
    source_key      VARCHAR(255)  NOT NULL,
    event_key       VARCHAR(255)  NOT NULL,
    event_id        CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    payload         NVARCHAR(MAX) NOT NULL,
    error           NVARCHAR(MAX) NOT NULL,
    attempt         INTEGER       NOT NULL CONSTRAINT omnicore_integration_failures_attempt_default DEFAULT 1,
    first_seen_at   DATETIME2(6)  NOT NULL CONSTRAINT omnicore_integration_failures_first_seen_default DEFAULT CURRENT_TIMESTAMP,
    last_attempt_at DATETIME2(6)  NOT NULL CONSTRAINT omnicore_integration_failures_last_attempt_default DEFAULT CURRENT_TIMESTAMP,
    resolved_at     DATETIME2(6)  NULL,
    CONSTRAINT omnicore_integration_failures_pkey PRIMARY KEY CLUSTERED (id),
    CONSTRAINT omnicore_integration_failures_attempt_positive CHECK (attempt > 0),
    CONSTRAINT omnicore_integration_failures_natural_key
        UNIQUE (consumer_group, source_key, event_key, event_id)
);
CREATE INDEX omnicore_integration_failures_pending_idx
    ON omnicore_integration_failures (consumer_group, source_key, event_key);
CREATE INDEX omnicore_integration_failures_last_attempt_idx
    ON omnicore_integration_failures (last_attempt_at);

-- ── omnicore_integration_processed ────────────────────────────────────────────
-- Consumer-side dedup: one row per (event_id, consumer_group) processed.
-- At-least-once delivery — handlers must still be idempotent.
CREATE TABLE omnicore_integration_processed (
    id              BINARY(16)    NOT NULL,
    event_id        CHAR(36)      COLLATE Latin1_General_100_BIN2 NOT NULL,
    consumer_group  VARCHAR(255)  NOT NULL,
    source_key      VARCHAR(255)  NOT NULL,
    event_key       VARCHAR(255)  NOT NULL,
    topic           VARCHAR(255)  NOT NULL,
    event_type      NVARCHAR(255) NOT NULL,
    processed_at    DATETIME2(6)  NOT NULL CONSTRAINT omnicore_integration_processed_at_default DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT omnicore_integration_processed_pkey PRIMARY KEY CLUSTERED (id),
    CONSTRAINT omnicore_integration_processed_natural_key UNIQUE (event_id, consumer_group)
);
CREATE INDEX omnicore_integration_processed_processed_at_idx
    ON omnicore_integration_processed (processed_at);
