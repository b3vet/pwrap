-- Control plane schema for pwrapd. Owns: projects, api_keys, migration_log.
-- All statements are idempotent so pwrapd can apply this on every startup.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS projects (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name              TEXT        NOT NULL UNIQUE,
    slug              TEXT        NOT NULL UNIQUE,
    pg_role           TEXT        NOT NULL,
    pg_schema         TEXT        NOT NULL,
    -- Encrypted at rest with AES-256-GCM when PWRAP_ENCRYPTION_KEY is set, using
    -- the envelope format `v1:<base64>`. Cleartext otherwise (pwrapd warns loudly
    -- at startup, and re-encrypts legacy plaintext rows once a key is configured).
    pg_password       TEXT        NOT NULL,
    -- parent_project_id points to the project this one was branched from. NULL for
    -- "root" projects. Used to support `pwrap branch` workflows.
    parent_project_id UUID        REFERENCES projects(id) ON DELETE SET NULL,
    metadata          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS projects_deleted_at_idx
    ON projects (deleted_at) WHERE deleted_at IS NULL;

-- API keys. Only the argon2id hash is stored; plaintext is returned once at issue.
CREATE TABLE IF NOT EXISTS api_keys (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    prefix       TEXT        NOT NULL,        -- first 8 chars of plaintext, for UI display
    hash         TEXT        NOT NULL,        -- argon2id encoded string
    name         TEXT        NOT NULL DEFAULT '',
    scopes       TEXT[]      NOT NULL DEFAULT '{}',
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX        IF NOT EXISTS api_keys_project_id_idx ON api_keys (project_id);
CREATE UNIQUE INDEX IF NOT EXISTS api_keys_prefix_idx     ON api_keys (prefix) WHERE revoked_at IS NULL;

-- Migration log records every tenant migration applied.
-- Records source + checksum (not just version int) so a future atlas/declarative
-- mode can co-exist without a schema rewrite.
CREATE TABLE IF NOT EXISTS migration_log (
    id            BIGSERIAL   PRIMARY KEY,
    project_id    UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version       TEXT        NOT NULL,
    source        TEXT        NOT NULL,      -- "embedded" | "atlas" | ...
    checksum      TEXT        NOT NULL,
    state         TEXT        NOT NULL,      -- "pending" | "applied" | "failed" | "index-building"
    error         TEXT,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS migration_log_project_id_idx
    ON migration_log (project_id, started_at DESC);

-- Ephemeral login roles minted by POST /v1/connection.
--
-- Each exchange creates a short-lived Postgres role that inherits the project's
-- tenant role and carries a server-enforced VALID UNTIL, so a leaked DSN stops
-- working on its own rather than relying on a client to respect an expiry hint.
-- This table is the sweeper's work list: pwrapd drops roles past their expiry
-- and deletes the row. Rows are the record, Postgres is the authority — a row
-- for a role that no longer exists is harmless and gets cleaned up.
CREATE TABLE IF NOT EXISTS ephemeral_roles (
    role_name  TEXT        PRIMARY KEY,
    project_id UUID        NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The sweeper's only query shape: everything already expired.
CREATE INDEX IF NOT EXISTS ephemeral_roles_expires_at_idx
    ON ephemeral_roles (expires_at);

-- Dropping a project's roles is a per-project operation.
CREATE INDEX IF NOT EXISTS ephemeral_roles_project_id_idx
    ON ephemeral_roles (project_id);

-- Scoped admin tokens.
--
-- PWRAP_BOOTSTRAP_TOKEN is reduced to a root credential whose only job is
-- minting these; every other management endpoint requires a token from this
-- table. Storage mirrors api_keys deliberately — argon2id hash, an 8-character
-- lookup prefix in the clear, plaintext returned once at issue — so there is one
-- credential implementation to review rather than two.
CREATE TABLE IF NOT EXISTS admin_tokens (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    prefix       TEXT        NOT NULL UNIQUE,
    token_hash   TEXT        NOT NULL,
    name         TEXT        NOT NULL DEFAULT '',
    -- Coarse capabilities: projects, keys, migrate, sql. `sql` is separable on
    -- purpose — it runs arbitrary SQL as the tenant role, so it is the one
    -- capability most worth withholding.
    scopes       TEXT[]      NOT NULL DEFAULT '{}',
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS admin_tokens_revoked_at_idx
    ON admin_tokens (revoked_at);

-- Audit trail for mutating admin calls.
--
-- Records the token prefix rather than the token: enough to identify the actor
-- and correlate with admin_tokens, useless to anyone who reads the table. Rows
-- are written whether the call succeeded or was refused, since a run of denials
-- is exactly what an investigation wants to see.
CREATE TABLE IF NOT EXISTS admin_audit_log (
    id            BIGSERIAL   PRIMARY KEY,
    token_prefix  TEXT,
    token_name    TEXT,
    action        TEXT        NOT NULL,
    method        TEXT        NOT NULL,
    path          TEXT        NOT NULL,
    project_id    UUID,
    status        INTEGER     NOT NULL,
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS admin_audit_log_created_at_idx
    ON admin_audit_log (created_at DESC);

CREATE INDEX IF NOT EXISTS admin_audit_log_token_prefix_idx
    ON admin_audit_log (token_prefix, created_at DESC);
