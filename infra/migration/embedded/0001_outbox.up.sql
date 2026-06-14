CREATE TABLE outbox (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  VARCHAR(100) NOT NULL,
    event_type      VARCHAR(50)  NOT NULL,
    aggregate_id    UUID         NOT NULL,
    payload         JSONB        NOT NULL,
    created_at      TIMESTAMP    NOT NULL DEFAULT NOW()
);

CREATE INDEX outbox_aggregate_idx  ON outbox (aggregate_type, aggregate_id);
CREATE INDEX outbox_created_at_idx ON outbox (created_at);

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

CREATE INDEX omnicore_mongo_views_applied_at_idx
    ON omnicore_mongo_views (applied_at DESC);

CREATE INDEX omnicore_mongo_views_status_idx
    ON omnicore_mongo_views (status)
    WHERE status <> 'done';

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

CREATE INDEX omnicore_upstream_failures_pending_idx
    ON omnicore_upstream_failures (subscription_topic, view_name, upstream_id)
    WHERE resolved_at IS NULL;

CREATE INDEX omnicore_upstream_failures_last_attempt_idx
    ON omnicore_upstream_failures (last_attempt_at DESC)
    WHERE resolved_at IS NULL;

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
    occurred_at   TIMESTAMP    NOT NULL,
    created_at    TIMESTAMP    NOT NULL DEFAULT NOW(),
    payload       JSONB        NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE audit_events_default PARTITION OF audit_events DEFAULT;

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
