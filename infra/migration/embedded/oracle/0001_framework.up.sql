-- ╔══════════════════════════════════════════════════════════════════════════╗
-- ║ OmniCore framework control plane — Oracle                                  ║
-- ╠══════════════════════════════════════════════════════════════════════════╣
-- ║ The Oracle flavor of the same logical schema as embedded/postgres,         ║
-- ║ embedded/mysql and embedded/sqlserver. Floor: Oracle Database 23ai (native ║
-- ║ BOOLEAN/JSON, IF [NOT] EXISTS). Every PK is a UUID v7 minted in Go, stored ║
-- ║ RAW(16) (the framework id standard); wire-crossing id references stay      ║
-- ║ VARCHAR2(36) text (CHAR(36) rejected — blank padding). JSONB/JSON→JSON     ║
-- ║ (native) EXCEPT the two CDC-tailed payloads (outbox, integration_events),  ║
-- ║ which are CLOB: LogMiner cannot decode native-JSON (OSON) redo, so what    ║
-- ║ Debezium reads must be text. TIMESTAMP/DATETIME→TIMESTAMP(6) + DEFAULT     ║
-- ║ SYSTIMESTAMP, TEXT→CLOB, VARCHAR(n)→VARCHAR2(n CHAR). PG's partial indexes and BRIN become    ║
-- ║ plain B-trees; no partitioning (plain audit table, like MySQL/SQL Server). ║
-- ║ Identifiers are UNQUOTED (folding to the same UPPERCASE catalog names the ║
-- ║ engine's quoted-uppercase identifiers address; a reserved-word collision   ║
-- ║ would be quoted UPPERCASE — none here); the engine lowercases them back at ║
-- ║ its read/error seams. PK/UNIQUE/CHECK constraint names are IDENTICAL       ║
-- ║ across all four dialects so a ConstraintBinding maps the same violation to ║
-- ║ the same HTTP status on every backend. Column semantics are documented in  ║
-- ║ the sibling files (MySQL carries per-column COMMENTs; COMMENT ON here      ║
-- ║ would triple the file).                                                    ║
-- ║ One deliberate Oracle rule carried by the sibling 0003 file: empty-string  ║
-- ║ text columns are NULLABLE with no default (elsewhere NOT NULL DEFAULT ''). ║
-- ║ Oracle stores '' as NULL, so an empty value CANNOT satisfy a NOT NULL      ║
-- ║ column; natural-key UNIQUEs still dedup those rows and the engine's MERGE  ║
-- ║ upsert compares conflict columns NULL-safely.                              ║
-- ╚══════════════════════════════════════════════════════════════════════════╝

-- ── outbox ──────────────────────────────────────────────────────────────────
-- One row per aggregate write, in-TX with the data row; tailed by Debezium
-- (LogMiner CDC on this table) and routed to the broker.
CREATE TABLE outbox (
    id              RAW(16)            NOT NULL,
    aggregate_type  VARCHAR2(100 CHAR) NOT NULL,
    event_type      VARCHAR2(50 CHAR)  NOT NULL,
    aggregate_id    VARCHAR2(36)       NOT NULL,
    -- CLOB, not native JSON, on purpose: LogMiner cannot decode the OSON
    -- binary redo of a JSON column (any Debezium), so a CDC-tailed payload
    -- must be text — the same wire-crossing rule that keeps ids uuid TEXT.
    payload         CLOB               NOT NULL,
    traceparent     VARCHAR2(64)       NULL,
    created_at      TIMESTAMP(6)       DEFAULT SYSTIMESTAMP NOT NULL,
    CONSTRAINT outbox_pkey PRIMARY KEY (id)
);
CREATE INDEX outbox_created_at_idx ON outbox (created_at);

-- ── omnicore_mongo_views ──────────────────────────────────────────────────────
-- Mongo read-side registry: declared shape + rebuild state per view (drift
-- detection + crash recovery).
CREATE TABLE omnicore_mongo_views (
    id                     RAW(16)            NOT NULL,
    view_name              VARCHAR2(255 CHAR) NOT NULL,
    version                NUMBER(10)         NOT NULL,
    rebuild_hash           VARCHAR2(64)       NOT NULL,
    artifact_hash          VARCHAR2(64)       NOT NULL,
    combined_hash          VARCHAR2(64)       NOT NULL,
    previous_version       NUMBER(10)         NULL,
    previous_combined_hash VARCHAR2(64)       NULL,
    previous_applied_at    TIMESTAMP(6)       NULL,
    status                 VARCHAR2(16)       DEFAULT 'done' NOT NULL,
    started_at             TIMESTAMP(6)       NULL,
    pid                    VARCHAR2(64)       NULL,
    host                   VARCHAR2(255 CHAR) NULL,
    applied_at             TIMESTAMP(6)       NOT NULL,
    applied_by             VARCHAR2(255 CHAR) NOT NULL,
    code_version           VARCHAR2(255 CHAR) NULL,
    CONSTRAINT omnicore_mongo_views_pkey PRIMARY KEY (id),
    CONSTRAINT omnicore_mongo_views_view_name_key UNIQUE (view_name),
    CONSTRAINT omnicore_mongo_views_version_positive CHECK (version > 0),
    CONSTRAINT omnicore_mongo_views_status_valid     CHECK (status IN ('done', 'processing'))
);
CREATE INDEX omnicore_mongo_views_applied_at_idx ON omnicore_mongo_views (applied_at);
CREATE INDEX omnicore_mongo_views_status_idx     ON omnicore_mongo_views (status);

-- ── audit_events ──────────────────────────────────────────────────────────────
-- Authoritative audit trail: one row per write, in-TX with the data row. Plain
-- table (no partitioning). The id is a time-ordered UUID v7 stored RAW(16), so the
-- primary-key index grows at the right edge (append-only insert locality).
CREATE TABLE audit_events (
    id            RAW(16)            NOT NULL,
    aggregate_id  VARCHAR2(36)       NOT NULL,
    entity_type   VARCHAR2(255 CHAR) NOT NULL,
    verb          VARCHAR2(32)       NOT NULL,
    action_name   VARCHAR2(64)       NOT NULL,
    kind          VARCHAR2(16)       NOT NULL,
    actor         VARCHAR2(255 CHAR) NULL,
    actor_issuer  VARCHAR2(255 CHAR) NULL,
    tenant_id     VARCHAR2(255 CHAR) NULL,
    thread_id     VARCHAR2(36)       NOT NULL,
    trace_id      VARCHAR2(32)       NULL,
    occurred_at   TIMESTAMP(6)       NOT NULL,
    created_at    TIMESTAMP(6)       DEFAULT SYSTIMESTAMP NOT NULL,
    payload       JSON               NOT NULL,
    CONSTRAINT audit_events_pkey PRIMARY KEY (id)
);
-- Only the entity-timeline index (serves FindByAggregate); FindByID is served by
-- the PK. audit_events is written in every write's TX, so it carries the minimum
-- index set — forensic indexes (actor/tenant/thread/time) are ad-hoc, added by
-- devops when a deployment needs them.
CREATE INDEX audit_events_entity_timeline_idx ON audit_events (entity_type, aggregate_id, occurred_at DESC);

-- ── integration_events ────────────────────────────────────────────────────────
-- Producer-side authoritative store of cross-service integration events (in-TX
-- under WithTx). Forensic timeline + replay source.
CREATE TABLE integration_events (
    id              RAW(16)            NOT NULL,
    event_id        VARCHAR2(36)       NOT NULL,
    aggregate_type  VARCHAR2(100 CHAR) NULL,
    aggregate_id    VARCHAR2(36)       NULL,
    event_type      VARCHAR2(100 CHAR) NOT NULL,
    event_version   NUMBER(10)         DEFAULT 1 NOT NULL,
    -- CLOB, not native JSON: this table is CDC-tailed (see outbox.payload).
    payload         CLOB               NOT NULL,
    correlation_id  VARCHAR2(36)       NULL,
    causation_id    VARCHAR2(36)       NULL,
    thread_id       VARCHAR2(36)       NOT NULL,
    traceparent     VARCHAR2(64)       NULL,
    actor           VARCHAR2(255 CHAR) NOT NULL,
    created_at      TIMESTAMP(6)       DEFAULT SYSTIMESTAMP NOT NULL,
    CONSTRAINT integration_events_pkey PRIMARY KEY (id),
    CONSTRAINT integration_events_event_id_uniq UNIQUE (event_id)
);
CREATE INDEX integration_events_aggregate_idx  ON integration_events (aggregate_type, aggregate_id, created_at);
CREATE INDEX integration_events_event_type_idx ON integration_events (event_type, created_at);

-- ── omnicore_integration_failures ─────────────────────────────────────────────
-- Consumer-side integration handler failure registry; retried by
-- Receiver.RetryPendingFailures.
CREATE TABLE omnicore_integration_failures (
    id              RAW(16)       NOT NULL,
    consumer_group  VARCHAR2(255) NOT NULL,
    source_key      VARCHAR2(255) NOT NULL,
    event_key       VARCHAR2(255) NOT NULL,
    event_id        VARCHAR2(36)  NOT NULL,
    payload         JSON          NOT NULL,
    error           CLOB          NOT NULL,
    attempt         NUMBER(10)    DEFAULT 1 NOT NULL,
    first_seen_at   TIMESTAMP(6)  DEFAULT SYSTIMESTAMP NOT NULL,
    last_attempt_at TIMESTAMP(6)  DEFAULT SYSTIMESTAMP NOT NULL,
    resolved_at     TIMESTAMP(6)  NULL,
    CONSTRAINT omnicore_integration_failures_pkey PRIMARY KEY (id),
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
    id              RAW(16)            NOT NULL,
    event_id        VARCHAR2(36)       NOT NULL,
    consumer_group  VARCHAR2(255 CHAR) NOT NULL,
    source_key      VARCHAR2(255 CHAR) NOT NULL,
    event_key       VARCHAR2(255 CHAR) NOT NULL,
    topic           VARCHAR2(255 CHAR) NOT NULL,
    event_type      VARCHAR2(255 CHAR) NOT NULL,
    processed_at    TIMESTAMP(6)       DEFAULT SYSTIMESTAMP NOT NULL,
    CONSTRAINT omnicore_integration_processed_pkey PRIMARY KEY (id),
    CONSTRAINT omnicore_integration_processed_natural_key UNIQUE (event_id, consumer_group)
);
CREATE INDEX omnicore_integration_processed_processed_at_idx
    ON omnicore_integration_processed (processed_at);
