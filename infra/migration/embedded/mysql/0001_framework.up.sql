-- ╔══════════════════════════════════════════════════════════════════════════╗
-- ║ OmniCore framework control plane — MySQL                                   ║
-- ╠══════════════════════════════════════════════════════════════════════════╣
-- ║ The MySQL flavor of the same logical schema as embedded/postgres. Every    ║
-- ║ PK is a UUID v7 minted in Go, stored BINARY(16) (the framework id          ║
-- ║ standard); wire-crossing id references stay CHAR(36) text. Dialect         ║
-- ║ differences: UUID→CHAR(36), JSONB→JSON, BIGSERIAL→BIGINT AUTO_INCREMENT,   ║
-- ║ TIMESTAMP+NOW()→DATETIME+CURRENT_TIMESTAMP. MySQL has no table partitioning ║
-- ║ by a DEFAULT range, no BRIN, and no partial indexes, so audit_events is a   ║
-- ║ plain table and the partial/BRIN indexes become plain btrees (functionally  ║
-- ║ equivalent, operationally simpler). Every table/column carries a COMMENT.   ║
-- ╚══════════════════════════════════════════════════════════════════════════╝

-- ── outbox ──────────────────────────────────────────────────────────────────
CREATE TABLE outbox (
    id              BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard). Not the business key — see aggregate_id.',
    aggregate_type  VARCHAR(100) NOT NULL COMMENT 'Logical aggregate/table name (e.g. users); Debezium routes it to topic <aggregate_type>.events.',
    event_type      VARCHAR(50)  NOT NULL COMMENT 'Lifecycle verb: INSERTED | UPDATED | ARCHIVED | UNARCHIVED | DELETED. Carried as a Kafka header.',
    aggregate_id    CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'Id of the written aggregate root (UUID text); becomes the Kafka message key.',
    payload         JSON         NOT NULL COMMENT 'JSON snapshot of the write (root fields + active children). Informational — the SyncEngine re-reads from the DB.',
    traceparent     VARCHAR(64)  NULL     COMMENT 'W3C traceparent of the producing request; mapped to a Kafka header. NULL when tracing is off.',
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Insert timestamp; drives the created_at index used for pruning/replay.',
    PRIMARY KEY (id),
    KEY outbox_created_at_idx (created_at)
) COMMENT='Transactional outbox: one row per aggregate write, in-TX with the data row; tailed by Debezium and routed to Kafka.';

-- ── omnicore_mongo_views ──────────────────────────────────────────────────────
CREATE TABLE omnicore_mongo_views (
    id                     BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard).',
    view_name              VARCHAR(255) NOT NULL COMMENT 'Mongo collection / view name — the natural key (UNIQUE); every lookup keys on it.',
    version                INTEGER      NOT NULL COMMENT 'Developer-declared ViewDefinition.Version(N); bumped on any rebuild-relevant shape change.',
    rebuild_hash           VARCHAR(64)  NOT NULL COMMENT 'SHA-256 over rebuild-relevant state (root, embeds, DeleteOnArchive, schema, collation, capped, time-series).',
    artifact_hash          VARCHAR(64)  NOT NULL COMMENT 'SHA-256 over indexes only — applied without a doc rewrite.',
    combined_hash          VARCHAR(64)  NOT NULL COMMENT 'SHA-256 identity stamped on the row; the value drift detection compares.',
    previous_version       INTEGER      NULL     COMMENT 'Version before the last rebuild (rollback/forensics).',
    previous_combined_hash VARCHAR(64)  NULL     COMMENT 'combined_hash before the last rebuild.',
    previous_applied_at    DATETIME     NULL     COMMENT 'applied_at before the last rebuild.',
    status                 VARCHAR(16)  NOT NULL DEFAULT 'done' COMMENT 'Rebuild state machine: done | processing.',
    started_at             DATETIME     NULL     COMMENT 'When the in-flight rebuild began; NULL when status=done.',
    pid                    VARCHAR(64)  NULL     COMMENT 'OS pid holding the rebuild (forensics).',
    host                   VARCHAR(255) NULL     COMMENT 'Host holding the rebuild (forensics).',
    applied_at             DATETIME     NOT NULL COMMENT 'When the current shape was last applied.',
    applied_by             VARCHAR(255) NOT NULL COMMENT 'Who applied it: <svc>@pid:<n> or manual-reconcile-*.',
    code_version           VARCHAR(255) NULL     COMMENT 'OMNICORE_CODE_VERSION env at apply time (build provenance).',
    PRIMARY KEY (id),
    CONSTRAINT omnicore_mongo_views_view_name_key UNIQUE (view_name),
    KEY omnicore_mongo_views_applied_at_idx (applied_at),
    KEY omnicore_mongo_views_status_idx (status),
    CONSTRAINT omnicore_mongo_views_version_positive CHECK (version > 0),
    CONSTRAINT omnicore_mongo_views_status_valid     CHECK (status IN ('done', 'processing'))
) COMMENT='Mongo read-side registry: declared shape + rebuild state per view (drift detection + crash recovery).';

-- ── omnicore_upstream_failures ────────────────────────────────────────────────
CREATE TABLE omnicore_upstream_failures (
    id                  BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard).',
    subscription_topic  VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Upstream Kafka topic the subscription consumes. (ascii VARCHAR(255): technical identifier; covers Kafka''s 249-char topic limit. With the other natural-key columns the composite UNIQUE stays well under MySQL''s 3072-byte index limit.)',
    view_name           VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Local view whose recompose failed.',
    upstream_id         VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Id of the changed upstream document that triggered the recompose (UUID / external id).',
    local_id            VARCHAR(255) CHARACTER SET ascii NOT NULL DEFAULT '' COMMENT 'Id of the local doc being recomposed; empty on the discover stage.',
    stage               VARCHAR(16)  NOT NULL COMMENT 'Where it failed: discover | compose | upsert.',
    error               TEXT         NOT NULL COMMENT 'Last error message (overwritten per retry).',
    attempt             INTEGER      NOT NULL DEFAULT 1 COMMENT 'Retry counter; auto-incremented on conflict.',
    first_seen_at       DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When first recorded (frozen).',
    last_attempt_at     DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When last retried (refreshed).',
    resolved_at         DATETIME     NULL     COMMENT 'Set when a clean recompose pass succeeds; NULL while pending.',
    PRIMARY KEY (id),
    KEY omnicore_upstream_failures_pending_idx (subscription_topic, view_name, upstream_id),
    KEY omnicore_upstream_failures_last_attempt_idx (last_attempt_at),
    CONSTRAINT omnicore_upstream_failures_stage_valid    CHECK (stage IN ('discover', 'compose', 'upsert')),
    CONSTRAINT omnicore_upstream_failures_attempt_positive CHECK (attempt > 0),
    CONSTRAINT omnicore_upstream_failures_natural_key
        UNIQUE (subscription_topic, view_name, upstream_id, local_id, stage)
) COMMENT='Cross-service recompose failure registry (mirrors live state); retried by UpstreamSubscriber.RetryPendingFailures.';

-- ── audit_events ──────────────────────────────────────────────────────────────
-- Plain table on MySQL (no partitioning). The id is a time-ordered UUID v7 stored
-- BINARY(16), so the clustered primary key gives append-only insert locality.
-- Only the entity-timeline index is carried (it serves FindByAggregate); FindByID
-- is served by the PK, and forensic lookups are ad-hoc indexes devops adds.
CREATE TABLE audit_events (
    id            BINARY(16)   NOT NULL COMMENT 'Audit row id — UUID v7 minted in Go (the framework id standard).',
    aggregate_id  CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'Id of the audited aggregate root.',
    entity_type   VARCHAR(255) NOT NULL COMMENT 'Entity/aggregate Go type name (e.g. User).',
    verb          VARCHAR(32)  NOT NULL COMMENT 'SQL-grounded verb: insert | update | archive | unarchive | delete.',
    action_name   VARCHAR(64)  NOT NULL COMMENT 'The Get* action that produced it (carries the PUT-vs-PATCH intent).',
    kind          VARCHAR(16)  NOT NULL COMMENT 'Body regime: snapshot | delta | transition.',
    actor         VARCHAR(255) NULL     COMMENT 'Authenticated principal (JWT sub) or anonymous; NULL only with no actor context.',
    actor_issuer  VARCHAR(255) NULL     COMMENT 'JWT issuer (iss); NULL when no token.',
    tenant_id     VARCHAR(255) NULL     COMMENT 'Tenant claim when multi-tenant; NULL otherwise.',
    thread_id     CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'Request id (AppContext.ID) tying the row to logs/traces.',
    trace_id      VARCHAR(32)  NULL     COMMENT 'Pivot column: 32-char hex trace id of the producing request. NULL when tracing is off.',
    occurred_at   DATETIME     NOT NULL COMMENT 'When the domain operation happened (business time).',
    created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When the row was written.',
    payload       JSON         NOT NULL COMMENT 'JSON body: snapshot | changes (delta) | children, per kind.',
    PRIMARY KEY (id),
    KEY audit_events_entity_timeline_idx (entity_type, aggregate_id, occurred_at DESC)
) COMMENT='Authoritative audit trail: one row per write, in-TX with the data row.';

-- ── integration_events ────────────────────────────────────────────────────────
CREATE TABLE integration_events (
    id              BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard).',
    event_id        CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'Globally-unique event id (UUID text); the consumer-side dedup key.',
    aggregate_type  VARCHAR(100) NULL     COMMENT 'Aggregate type the event is about; NULL for a standalone event.',
    aggregate_id    CHAR(36)     CHARACTER SET ascii NULL     COMMENT 'Aggregate id the event is about; NULL for a standalone event.',
    event_type      VARCHAR(100) NOT NULL COMMENT 'Wire event type header value (e.g. UserActivated).',
    event_version   INTEGER      NOT NULL DEFAULT 1 COMMENT 'Schema version of the event payload.',
    payload         JSON         NOT NULL COMMENT 'JSON event payload (the cross-service contract).',
    correlation_id  CHAR(36)     CHARACTER SET ascii NULL     COMMENT 'Correlation id of the originating request chain (== trace id); NULL if none.',
    causation_id    CHAR(36)     CHARACTER SET ascii NULL     COMMENT 'Id of the event/command that caused this one; NULL if none.',
    thread_id       CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'Producing request id (AppContext.ID).',
    traceparent     VARCHAR(64)  NULL     COMMENT 'W3C traceparent of the producing request; links the async consumer span. NULL when tracing is off.',
    actor           VARCHAR(255) NOT NULL COMMENT 'Producing principal (JWT sub) or anonymous; never empty.',
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'Insert timestamp.',
    PRIMARY KEY (id),
    UNIQUE KEY integration_events_event_id_uniq (event_id),
    KEY integration_events_aggregate_idx (aggregate_type, aggregate_id, created_at),
    KEY integration_events_event_type_idx (event_type, created_at)
) COMMENT='Producer-side authoritative store of cross-service integration events (in-TX under WithTx). Forensic timeline + replay source.';

-- ── omnicore_integration_failures ─────────────────────────────────────────────
CREATE TABLE omnicore_integration_failures (
    id              BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard).',
    consumer_group  VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Kafka consumer group that failed to process the event. (ascii VARCHAR(255): technical identifier; the composite UNIQUE with source_key/event_key/event_id stays well under MySQL''s 3072-byte index limit.)',
    source_key      VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Go-side source identifier (the From key), stable across wire-topic renames.',
    event_key       VARCHAR(255) CHARACTER SET ascii NOT NULL COMMENT 'Go-side event identifier (the On key).',
    event_id        CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'The failing event id (UUID text).',
    payload         JSON         NOT NULL COMMENT 'JSON payload of the failing event (for retry/inspection).',
    error           TEXT         NOT NULL COMMENT 'Last error message (overwritten per retry).',
    attempt         INTEGER      NOT NULL DEFAULT 1 COMMENT 'Retry counter; auto-incremented on conflict.',
    first_seen_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When first recorded (frozen).',
    last_attempt_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When last retried (refreshed).',
    resolved_at     DATETIME     NULL     COMMENT 'Set when the event is finally processed; NULL while pending.',
    PRIMARY KEY (id),
    KEY omnicore_integration_failures_pending_idx (consumer_group, source_key, event_key),
    KEY omnicore_integration_failures_last_attempt_idx (last_attempt_at),
    CONSTRAINT omnicore_integration_failures_attempt_positive CHECK (attempt > 0),
    CONSTRAINT omnicore_integration_failures_natural_key
        UNIQUE (consumer_group, source_key, event_key, event_id)
) COMMENT='Consumer-side integration handler failure registry; retried by Receiver.RetryPendingFailures.';

-- ── omnicore_integration_processed ────────────────────────────────────────────
CREATE TABLE omnicore_integration_processed (
    id              BINARY(16)   NOT NULL COMMENT 'Surrogate row id — UUID v7 minted in Go (the framework id standard).',
    event_id        CHAR(36)     CHARACTER SET ascii NOT NULL COMMENT 'Processed event id, canonical uuid TEXT (part of the UNIQUE natural key).',
    consumer_group  VARCHAR(255) NOT NULL COMMENT 'Consumer group that processed it (part of the UNIQUE natural key; groups dedup independently).',
    source_key      VARCHAR(255) NOT NULL COMMENT 'Go-side source identifier (diagnostics).',
    event_key       VARCHAR(255) NOT NULL COMMENT 'Go-side event identifier (diagnostics).',
    topic           VARCHAR(255) NOT NULL COMMENT 'Kafka topic the event came from (diagnostics).',
    event_type      VARCHAR(255) NOT NULL COMMENT 'Wire event type (diagnostics).',
    processed_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT 'When processed; indexed for cheap pruning.',
    PRIMARY KEY (id),
    CONSTRAINT omnicore_integration_processed_natural_key UNIQUE (event_id, consumer_group),
    KEY omnicore_integration_processed_processed_at_idx (processed_at)
) COMMENT='Consumer-side dedup: one row per (event_id, consumer_group) processed. At-least-once delivery — handlers must still be idempotent.';
