-- Tenant baseline: built-in tables (JSONB documents, pgvector embeddings, PostGIS geo,
-- River queue) plus a matview registry. Applied per-project, into the project's schema,
-- as the project's role. Extensions (vector, pg_graphql, postgis) are created by
-- pwrapd's superuser connection before this runs, so the types they expose resolve here.

CREATE TABLE IF NOT EXISTS pwrap_documents (
    collection  TEXT        NOT NULL,
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    data        JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pwrap_documents_collection_idx
    ON pwrap_documents (collection);
CREATE INDEX IF NOT EXISTS pwrap_documents_data_gin_idx
    ON pwrap_documents USING GIN (data jsonb_path_ops);
-- BRIN on created_at: append-mostly pattern, BRIN is orders of magnitude smaller than
-- a btree and good enough for time-range scans on this column.
CREATE INDEX IF NOT EXISTS pwrap_documents_created_at_brin
    ON pwrap_documents USING BRIN (created_at);

-- pwrap_embeddings is keyed by (collection, doc_id) with a separate surrogate id.
-- Dimension is fixed at 1536 (OpenAI default) for MVP; making it per-collection is a
-- post-MVP change that will need a new migration + a registry table.
CREATE TABLE IF NOT EXISTS pwrap_embeddings (
    collection  TEXT        NOT NULL,
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    doc_id      TEXT        NOT NULL,
    embedding   VECTOR(1536) NOT NULL,
    metadata    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (collection, doc_id)
);
CREATE INDEX IF NOT EXISTS pwrap_embeddings_collection_idx
    ON pwrap_embeddings (collection);
-- HNSW build here is fine for an empty tenant schema; a post-MVP migration can
-- rebuild CONCURRENTLY for non-empty tables and track `index-ready` state in migration_log.
CREATE INDEX IF NOT EXISTS pwrap_embeddings_hnsw_idx
    ON pwrap_embeddings USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS pwrap_embeddings_created_at_brin
    ON pwrap_embeddings USING BRIN (created_at);

-- pwrap_geo holds PostGIS geometries. Stored as GEOMETRY(Geometry, 4326) (WGS84) so the
-- table accepts points/polygons/lines/etc; cast to geography in queries that need
-- meter-accurate distance. GIST supports both bbox containment and KNN ordering (<->).
CREATE TABLE IF NOT EXISTS pwrap_geo (
    collection  TEXT                       NOT NULL,
    id          UUID                       PRIMARY KEY DEFAULT gen_random_uuid(),
    geom        GEOMETRY(Geometry, 4326)   NOT NULL,
    metadata    JSONB                      NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ                NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ                NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS pwrap_geo_collection_idx
    ON pwrap_geo (collection);
CREATE INDEX IF NOT EXISTS pwrap_geo_geom_gist
    ON pwrap_geo USING GIST (geom);
CREATE INDEX IF NOT EXISTS pwrap_geo_created_at_brin
    ON pwrap_geo USING BRIN (created_at);

-- pwrap_partition_ensure(parent, when_) idempotently creates a monthly partition
-- of `parent` covering the month containing `when_`. Designed for callers like:
--    SELECT pwrap_partition_ensure('events', now());                -- current month
--    SELECT pwrap_partition_ensure('events', now() + interval '1 month'); -- next month
-- Users create the parent as a RANGE-partitioned table on a TIMESTAMPTZ column and
-- run this from a River periodic job (or pg_cron, or app-level scheduler).
CREATE OR REPLACE FUNCTION pwrap_partition_ensure(
    parent_table TEXT,
    when_        TIMESTAMPTZ
) RETURNS TEXT
LANGUAGE plpgsql
AS $$
DECLARE
    start_ts  TIMESTAMPTZ := date_trunc('month', when_);
    end_ts    TIMESTAMPTZ := start_ts + interval '1 month';
    part_name TEXT        := parent_table || '_' || to_char(start_ts, 'YYYY_MM');
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
        part_name, parent_table, start_ts, end_ts
    );
    RETURN part_name;
END;
$$;

-- ---------------------------------------------------------------------------
-- Realtime change capture (Path R / M19)
-- ---------------------------------------------------------------------------
-- pwrap_change_log records every INSERT/UPDATE/DELETE on tables the SDK has
-- "subscribed". Triggers attach via pwrap_capture_change(); on each change the
-- trigger inserts a row here AND emits pg_notify('pwrap_changes', '{...}') with
-- the change_log row id, so subscribers wake instantly without polling.

CREATE TABLE IF NOT EXISTS pwrap_change_log (
    id          BIGSERIAL   PRIMARY KEY,
    schema_name TEXT        NOT NULL,
    table_name  TEXT        NOT NULL,
    op          TEXT        NOT NULL,         -- 'INSERT' | 'UPDATE' | 'DELETE'
    row_id      TEXT,                          -- to_jsonb(NEW/OLD)->>'id', NULL if no id col
    user_id     TEXT,                          -- request.jwt.claims->>'user_id' at trigger time
    before      JSONB,                         -- null for INSERT
    after       JSONB,                         -- null for DELETE
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- BRIN is enough for the common "give me recent events" scan pattern; events are
-- append-only and time-ordered. Subscribers don't query the log directly — pwrapd's
-- Hub fetches by id from the NOTIFY payload.
CREATE INDEX IF NOT EXISTS pwrap_change_log_created_at_brin
    ON pwrap_change_log USING BRIN (created_at);
CREATE INDEX IF NOT EXISTS pwrap_change_log_table_idx
    ON pwrap_change_log (table_name);
CREATE INDEX IF NOT EXISTS pwrap_change_log_user_idx
    ON pwrap_change_log (user_id) WHERE user_id IS NOT NULL;

-- pwrap_capture_change is the trigger function attached to every captured table.
-- Pulls user_id from request.jwt.claims (set by SDK Client.WithUser or PostgREST),
-- records the change, and emits a compact NOTIFY with the new log row's id so
-- pwrapd's Hub can pick up the rest by SELECTing the log row.
--
-- The INSERT into pwrap_change_log is schema-qualified via EXECUTE format(...).
-- A bare `INSERT INTO pwrap_change_log` would resolve through the calling
-- session's search_path, which doesn't include the tenant schema when the
-- trigger fires from admin-driven cross-schema work (e.g. branch data copy).
-- That'd error "relation pwrap_change_log does not exist" and roll back the
-- original INSERT.
CREATE OR REPLACE FUNCTION pwrap_capture_change() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    v_user_id  text;
    v_row_id   text;
    v_log_id   bigint;
    v_before   jsonb;
    v_after    jsonb;
BEGIN
    BEGIN
        v_user_id := NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id';
    EXCEPTION WHEN OTHERS THEN
        v_user_id := NULL;
    END;

    IF TG_OP = 'DELETE' THEN
        v_row_id := to_jsonb(OLD)->>'id';
    ELSE
        v_row_id := to_jsonb(NEW)->>'id';
    END IF;
    IF TG_OP IN ('UPDATE','DELETE') THEN v_before := to_jsonb(OLD); END IF;
    IF TG_OP IN ('INSERT','UPDATE') THEN v_after  := to_jsonb(NEW); END IF;

    EXECUTE format(
        'INSERT INTO %I.pwrap_change_log
            (schema_name, table_name, op, row_id, user_id, before, after)
         VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id',
        TG_TABLE_SCHEMA
    )
    USING TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP, v_row_id, v_user_id, v_before, v_after
    INTO v_log_id;

    -- NOTIFY payload is intentionally small (well under Postgres's ~8KB limit).
    -- Subscribers fetch the full row from pwrap_change_log by id.
    PERFORM pg_notify('pwrap_changes', json_build_object(
        'log_id', v_log_id,
        'schema', TG_TABLE_SCHEMA,
        'table',  TG_TABLE_NAME,
        'op',     TG_OP,
        'row_id', v_row_id,
        'user_id', v_user_id
    )::text);

    RETURN COALESCE(NEW, OLD);
END;
$$;

-- Auto-attach the capture trigger to pwrap_documents so the SDK's Table API is
-- "realtime by default." pwrap_embeddings, pwrap_geo, and user tables stay opt-in
-- via Client.EnableChangeCapture(ctx, "<table>").
DROP TRIGGER IF EXISTS pwrap_documents_capture ON pwrap_documents;
CREATE TRIGGER pwrap_documents_capture
    AFTER INSERT OR UPDATE OR DELETE
    ON pwrap_documents
    FOR EACH ROW
    EXECUTE FUNCTION pwrap_capture_change();

-- pwrap_matviews is the registry of materialized views the SDK's Matview API manages.
-- `definition` holds the body after `AS` so we can detect drift; `checksum` is a fast
-- idempotency check. Scheduling lives in user code (River periodic jobs); this table
-- records state + lets us report last_refresh_at / last_error.
CREATE TABLE IF NOT EXISTS pwrap_matviews (
    name            TEXT        PRIMARY KEY,
    definition      TEXT        NOT NULL,
    checksum        TEXT        NOT NULL,
    last_refresh_at TIMESTAMPTZ,
    last_error      TEXT,
    enabled         BOOLEAN     NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- PostgREST-exposed GraphQL entrypoint.
-- pg_graphql's `graphql.resolve` lives in the `graphql` schema, which pwrap does NOT
-- add to PostgREST's db-schemas list (keeping graphql.* out of every tenant's REST
-- surface). Instead, we expose a tenant-local wrapper function named `graphql` —
-- PostgREST picks it up automatically at /rpc/graphql.
CREATE OR REPLACE FUNCTION "graphql"(
    "operationName" text    DEFAULT NULL,
    query           text    DEFAULT NULL,
    variables       jsonb   DEFAULT NULL,
    extensions      jsonb   DEFAULT NULL
) RETURNS jsonb
LANGUAGE sql
VOLATILE
AS $$
    SELECT graphql.resolve(
        query          := query,
        variables      := COALESCE(variables, '{}'::jsonb),
        "operationName":= "operationName",
        extensions     := extensions
    );
$$;
