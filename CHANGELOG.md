# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the version is `0.x`, the public API may change in any release.

## [Unreleased]

## [0.1.0] - 2026-08-04

First public release.

### Added

- **JSONB document tables** over a single `pwrap_documents` table, with GIN indexes.
- **Durable queue** built on [River](https://github.com/riverqueue/river)
  (`SELECT … FOR UPDATE SKIP LOCKED`) — retries, DLQ, visibility.
- **Vector search** via [pgvector](https://github.com/pgvector/pgvector) with
  HNSW indexes, 1536 dimensions.
- **PostGIS geo collections** — `InsertPoint`, `Insert`, `WithinRadius`,
  `WithinBBox`, `Nearest` over `pwrap_geo`, with GIST (KNN + bbox).
- **BRIN indexes** on `created_at` for every built-in table, and a
  `pwrap_partition_ensure()` helper for idempotent monthly partitions.
- **Materialized view registry** — `Matview(name).Register/Refresh`, tracking
  `last_refresh_at` in `pwrap_matviews`.
- **Control plane** (`pwrapd`): projects, argon2id-hashed API keys, per-project
  migrations, and scoped DSN handoff.
- **Auto REST + GraphQL** per project via [PostgREST](https://postgrest.org) and
  [pg_graphql](https://github.com/supabase/pg_graphql); `POST /v1/rest/token`
  issues a short-lived HS256 JWT.
- **Row-level security helpers** — `WithUser(id)` wraps queries in a transaction
  with `SET LOCAL request.jwt.claims`, so one policy covers both the SDK and
  PostgREST.
- **`pwrap sql apply`** escape hatch for user-defined tables.
- **Project branching** — child projects with a fresh role, schema and baseline;
  optional data snapshot; idempotent `branch sync`.
- **Realtime** — change capture via trigger and `pg_notify`, plus WebSocket
  subscriptions at `GET /v1/subscribe`.
- **Encryption at rest** for `projects.pg_password` (AES-256-GCM, versioned
  `v1:<base64>` envelope), with startup re-encryption of legacy plaintext rows.
- **OpenTelemetry** tracing and metrics over OTLP/HTTP, plus a Prometheus
  `/metrics` endpoint.
- **Neon integration** — `pwrap neon branch create/list/delete` and
  `connection-uri` for per-PR preview databases.
- **SDKs** for Go, TypeScript and Python, kept at feature parity.

### Fixed

- Restored `cmd/pwrapd`, `cmd/pwrap`, `sdk/go/pwrap` and `sdk/py/pwrap`, which
  unanchored `.gitignore` patterns had excluded from version control. The
  patterns were meant to ignore compiled binaries but matched directories at any
  depth.
- Corrected the Go module path to `github.com/b3vet/pwrap`; the previous path
  pointed at a repository that does not exist, so the SDK could not be fetched.
- `sql apply` now notifies PostgREST to reload its schema cache. A table created
  through the escape hatch was invisible over REST — 404 on every request — until
  an unrelated project create or delete happened to trigger a reload.
- **TypeScript SDK**: `client.table()` is generic again, so
  `c.table<T>("name")` type-checks; `PwrapConfig.driver` accepts an async
  factory, without which the documented Neon edge-runtime setup could not
  compile; and `@neondatabase/serverless` is no longer bundled into
  `dist/neon.js` (215 KB → 388 B), restoring it to a genuinely optional
  dependency.
- Upgraded CI to golangci-lint v2, which is required for Go 1.25 modules.

### Testing

- The integration suite now runs PostgREST, covering REST tokens, CRUD, GraphQL,
  RLS and cross-tenant isolation — previously excluded entirely.
- New coverage for the queue, vector search, materialized views, and a migration
  down/up round trip. The `.down.sql` files had never been executed.
- A [cross-SDK conformance suite](conformance/README.md) runs the same scenarios
  through all three SDKs and fails the build on undeclared parity gaps.
- A nightly workflow runs the end-to-end demos, a Postgres 16/17 matrix, and a
  benchmark baseline.

### Known limitations

- The DSN from `/v1/connection` carries the tenant role's stable password. Its
  `expires_at` is a refresh hint, not an enforced expiry, and revoking an API key
  does not revoke a DSN already issued. See [SECURITY.md](SECURITY.md).
- `PWRAP_BOOTSTRAP_TOKEN` is a single unscoped admin credential with no audit trail.
- `projects`, `branches`, `realtime`, `sqlapply` and `authsetup` have no unit
  tests of their own; they are covered by the integration and conformance suites
  rather than directly.
- Branching copies built-in tables only. User-defined tables need their
  migrations re-run against the branch, then an explicit `sync`.
- The SDKs are not yet at full parity. Relative to Go: TypeScript lacks
  `withUser` (RLS scoping) and `matview`; Python lacks `subscribe`, `matview`
  and the change-capture toggles. Each gap is recorded in
  [`conformance/scenarios.json`](conformance/scenarios.json) and printed on every
  conformance run.

[Unreleased]: https://github.com/b3vet/pwrap/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/b3vet/pwrap/releases/tag/v0.1.0
