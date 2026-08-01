-- ╔══════════════════════════════════════════════════════════════════════════╗
-- ║ OmniCore framework control plane — SQLite                                  ║
-- ╠══════════════════════════════════════════════════════════════════════════╣
-- ║ SQLite translation of the framework control plane. Type affinities: UUID/  ║
-- ║ CHAR/VARCHAR → TEXT, JSONB → TEXT, TIMESTAMP → TEXT (RFC3339 / strftime),   ║
-- ║ BIGINT/INTEGER → INTEGER. DEFAULT NOW() → DEFAULT (strftime …%f) for ms     ║
-- ║ precision matching the app-clock. SQLite's default collation is BINARY, so  ║
-- ║ the Postgres COLLATE "C" byte-exact comparison is the default here. Partial ║
-- ║ indexes are supported natively; there is no BRIN (a plain B-tree stands in).║
-- ║ SQLite has no COMMENT ON, so the column docs live as -- comments.           ║
-- ╚══════════════════════════════════════════════════════════════════════════╝

-- ── outbox ──────────────────────────────────────────────────────────────────
-- Transactional outbox: one row per aggregate write, in-TX with the data row.
-- In the infra-free MVP posture nothing drains it (no CDC) — it accumulates,
-- the same eternal-accumulator behavior it has on every engine (the framework
-- never auto-prunes; operator-side pruning is the path).
CREATE TABLE outbox (
    id              TEXT    PRIMARY KEY,        -- surrogate row id (UUID v7, Go-minted)
    aggregate_type  TEXT    NOT NULL,           -- logical aggregate/table name
    event_type      TEXT    NOT NULL,           -- INSERTED | UPDATED | ARCHIVED | UNARCHIVED | DELETED
    aggregate_id    TEXT    NOT NULL,           -- written aggregate root id (canonical uuid text)
    payload         TEXT    NOT NULL,           -- JSON snapshot of the write
    traceparent     TEXT,                       -- W3C traceparent; NULL when tracing off
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);
CREATE INDEX outbox_created_at_idx ON outbox (created_at);

-- ── omnicore_mongo_views ─────────────────────────────────────────────────────
-- Mongo read-side registry: declared shape (hashes) + rebuild state per view.
-- Only exercised when Mongo projection is on; harmless in relational-only mode.
CREATE TABLE omnicore_mongo_views (
    id                     TEXT    PRIMARY KEY,
    view_name              TEXT    NOT NULL,
    version                INTEGER NOT NULL,
    rebuild_hash           TEXT    NOT NULL,
    artifact_hash          TEXT    NOT NULL,
    combined_hash          TEXT    NOT NULL,

    previous_version       INTEGER,
    previous_combined_hash TEXT,
    previous_applied_at    TEXT,

    status                 TEXT    NOT NULL DEFAULT 'done',
    started_at             TEXT,
    pid                    TEXT,
    host                   TEXT,

    applied_at             TEXT    NOT NULL,
    applied_by             TEXT    NOT NULL,
    code_version           TEXT,

    CONSTRAINT omnicore_mongo_views_version_positive CHECK (version > 0),
    CONSTRAINT omnicore_mongo_views_status_valid     CHECK (status IN ('done', 'processing')),
    CONSTRAINT omnicore_mongo_views_view_name_key    UNIQUE (view_name)
);
CREATE INDEX omnicore_mongo_views_applied_at_idx
    ON omnicore_mongo_views (applied_at DESC);
CREATE INDEX omnicore_mongo_views_status_idx
    ON omnicore_mongo_views (status)
    WHERE status <> 'done';

-- ── audit_events ─────────────────────────────────────────────────────────────
-- Authoritative audit trail: one row per write, in-TX with the data row when the
-- "database" destination is on. The v7 id is time-ordered → append-only locality.
CREATE TABLE audit_events (
    id            TEXT    PRIMARY KEY,           -- audit row id (UUID v7, Go-minted)
    aggregate_id  TEXT    NOT NULL,              -- audited aggregate root id
    entity_type   TEXT    NOT NULL,              -- entity/aggregate Go type name
    verb          TEXT    NOT NULL,              -- insert | update | archive | unarchive | delete
    action_name   TEXT    NOT NULL,              -- the Get* action that produced it
    kind          TEXT    NOT NULL,              -- snapshot | delta | transition
    actor         TEXT,                          -- authenticated principal (JWT sub) or "anonymous"
    actor_issuer  TEXT,                          -- JWT issuer (iss); NULL when no token
    tenant_id     TEXT,                          -- tenant claim; NULL otherwise
    thread_id     TEXT    NOT NULL,              -- request id (AppContext.ID)
    trace_id      TEXT,                          -- 32-char hex trace id; NULL when tracing off
    occurred_at   TEXT    NOT NULL,              -- business time
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
    payload       TEXT    NOT NULL               -- JSON body per kind
);
CREATE INDEX audit_events_entity_timeline_idx
    ON audit_events (entity_type, aggregate_id, occurred_at DESC);

-- ── integration_events ───────────────────────────────────────────────────────
-- Producer-side store of cross-service integration events (in-TX under WithTx).
CREATE TABLE integration_events (
    id              TEXT    PRIMARY KEY,
    event_id        TEXT    NOT NULL UNIQUE,     -- globally-unique event id (consumer dedup key)
    aggregate_type  TEXT,                        -- NULL for a standalone event
    aggregate_id    TEXT,                        -- NULL for a standalone event
    event_type      TEXT    NOT NULL,
    event_version   INTEGER NOT NULL DEFAULT 1,
    payload         TEXT    NOT NULL,
    correlation_id  TEXT,
    causation_id    TEXT,
    thread_id       TEXT    NOT NULL,
    traceparent     TEXT,
    actor           TEXT    NOT NULL,
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now'))
);
CREATE INDEX integration_events_aggregate_idx
    ON integration_events (aggregate_type, aggregate_id, created_at)
    WHERE aggregate_type IS NOT NULL;
CREATE INDEX integration_events_event_type_idx
    ON integration_events (event_type, created_at);

-- ── omnicore_integration_failures ────────────────────────────────────────────
-- Consumer-side integration failure registry; retried by RetryPendingFailures.
CREATE TABLE omnicore_integration_failures (
    id              TEXT    PRIMARY KEY,
    consumer_group  TEXT    NOT NULL,
    source_key      TEXT    NOT NULL,
    event_key       TEXT    NOT NULL,
    event_id        TEXT    NOT NULL,
    payload         TEXT    NOT NULL,
    error           TEXT    NOT NULL,
    attempt         INTEGER NOT NULL DEFAULT 1,
    first_seen_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
    last_attempt_at TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
    resolved_at     TEXT,

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

-- ── omnicore_integration_processed ───────────────────────────────────────────
-- Consumer-side dedup: one row per (event_id, consumer_group) processed.
-- Postgres BRIN-indexes processed_at for cheap pruning; SQLite has no BRIN, so a
-- plain B-tree index stands in (the retention DELETE is the same).
CREATE TABLE omnicore_integration_processed (
    id              TEXT    PRIMARY KEY,
    event_id        TEXT    NOT NULL,
    consumer_group  TEXT    NOT NULL,
    source_key      TEXT    NOT NULL,
    event_key       TEXT    NOT NULL,
    topic           TEXT    NOT NULL,
    event_type      TEXT    NOT NULL,
    processed_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f','now')),
    CONSTRAINT omnicore_integration_processed_natural_key UNIQUE (event_id, consumer_group)
);
CREATE INDEX omnicore_integration_processed_processed_at_idx
    ON omnicore_integration_processed (processed_at);
