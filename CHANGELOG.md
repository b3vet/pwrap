# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the version is `0.x`, the public API may change in any release.

## [Unreleased]

## [0.2.1] - 2026-09-14

### Added

- **`PWRAP_AUDIT_RETENTION_DAYS`** bounds the admin audit log. pwrapd prunes rows
  older than the window (90 days by default) on the same ten-minute housekeeping
  pass that sweeps expired DSN roles. `0` disables pruning and keeps rows
  forever. See [SECURITY.md](SECURITY.md) for how to pick the window.
- **`withUser` in the TypeScript SDK.** `client.withUser(id)` returns a derived
  client whose `table` / `vector` / `geo` / `queue` operations run inside a
  transaction with `request.jwt.claims` set, so one RLS policy covers both the
  SDK and PostgREST. The derived client shares the parent's connection and its
  credential refreshes; `close()` on it is a no-op. `subscribe()` inherits the
  bound user unless a `userId` is passed explicitly.
- **`matview` in the TypeScript and Python SDKs** — `register`, `refresh`,
  `refreshConcurrent` / `refresh_concurrent`, `drop` and `info`, against the same
  `pwrap_matviews` registry the Go SDK uses. All three compute the definition
  checksum identically, so registering from one SDK does not look stale to
  another.

### Changed

- The TypeScript handles now route every query through the client rather than
  capturing a connection, which is what makes `withUser` possible. No change to
  their public API.

### Fixed

- **A matview created through the SDK could not be refreshed afterwards.** The
  role that runs `CREATE` owns the result, and since 0.2.0 clients connect as a
  credential that expires within the hour — so `REFRESH`, which requires
  ownership, failed with `must be owner of materialized view` for any other
  client and for the same client after its own hourly credential rotation.
  `register` now hands the matview to the project's tenant role, which outlives
  every credential and which every connection inherits. Existing matviews are
  repaired the next time they are registered, or with
  `ALTER MATERIALIZED VIEW <name> OWNER TO <tenant role>`.
- **Deleting a project failed if a client had created anything.** Postgres
  refuses to drop a role that still owns objects, so a project whose client had
  registered a matview returned `500` on delete and its expired roles could
  never be swept. Ownership is now handed to the tenant role before an ephemeral
  role is dropped, and the delete drops the schema first.
- The last two cross-SDK parity gaps are closed. `conformance/scenarios.json`
  lists no `missing_from` entries, and the parity gate now enforces all 12
  scenarios across all three SDKs — including a new one that registers a matview
  with one client and refreshes it with a second, which is what caught the
  ownership bug.
- `scripts/refresh-check.sh` is re-runnable: it used a fixed project name, so a
  second run collided with the first and died with a bare `KeyError: id`. It now
  uses a unique name, reports what the control plane actually returned, and
  deletes its project on the way out.
- `openapi.yaml` advertised `version: 0.1.0`. Its paths were updated for the
  0.2.0 scope changes but the version field was not.
- The `todo-plus` demo now fails when its teardown fails. It only logged before,
  which is how the project-delete bug above stayed green in CI.


## [0.2.0] - 2026-09-14

**Two breaking changes.** Both are deliberate and both need action — see
*Migrating* below before upgrading.

### Breaking

- **The bootstrap token no longer authenticates the management API.**
  `PWRAP_BOOTSTRAP_TOKEN` is now the root credential, and its only power is
  minting scoped admin tokens. Every other `/v1` endpoint requires one of those
  instead. Existing automation that sends the bootstrap token to
  `/v1/projects`, `/v1/projects/{id}/keys`, `/v1/projects/{id}/migrations` or
  `/v1/projects/{id}/sql` will receive `401`.
- **DSN credentials now really expire.** `/v1/connection` mints a Postgres role
  with `VALID UNTIL` set to the TTL (one hour by default). SDK versions 0.1.x
  cache the DSN and never re-exchange, so they stop working an hour after
  connecting to a 0.2.0 server. 0.2.0 SDKs refresh automatically.

### Migrating

1. Mint an admin token and use it wherever the bootstrap token used to go:

   ```bash
   export PWRAP_BOOTSTRAP_TOKEN=...              # as before
   pwrap admin token issue --scopes projects,keys,migrate,sql --name ci
   export PWRAP_ADMIN_TOKEN=pwa_...              # printed once, not recoverable
   ```

   Grant the narrowest set that does the job. A pipeline that only runs
   migrations wants `--scopes migrate`, not all four.

2. Upgrade every SDK to 0.2.0 at the same time as the server. There is no
   compatibility window: a 0.1.x client connected to a 0.2.0 server works until
   its first credential expires, then fails to reconnect. Raising
   `PWRAP_DSN_TTL_SECONDS` buys time but does not fix it.

### Added

- **Rotating DSN credentials.** Each `/v1/connection` exchange creates its own
  login role inheriting the tenant role, with a server-enforced `VALID UNTIL`.
  A leaked DSN now stops working on its own, and one client's credential can be
  revoked without touching anyone else's. TTL via `PWRAP_DSN_TTL_SECONDS`.
  pwrapd sweeps expired roles.
- **SDK credential refresh** in Go, TypeScript and Python: re-exchange before
  expiry, swap the pool, close the old one after a grace period. A failed
  attempt keeps the current connection and retries.
- **Scoped admin tokens** (`pwa_…`) with four coarse scopes — `projects`,
  `keys`, `migrate`, `sql` — stored as argon2id hashes like project API keys.
  Managed with `pwrap admin token issue|list|revoke`.
- **Admin audit log.** Every mutating management call writes to
  `admin_audit_log`: acting token prefix and name, route, project, status.
  Refusals are recorded; reads are not.

### Changed

- The TypeScript SDK builds on TypeScript 7. Declarations are emitted by `tsc`
  rather than tsup's bundled plugin, so they cannot fall behind the compiler
  again.

### Fixed

- **CommonJS consumers get usable types.** Present in 0.1.0 and 0.1.1: the
  package is `"type": "module"`, so TypeScript read its `.d.ts` as ESM and a CJS
  consumer under `node16` hit TS1479 despite `dist/index.cjs` working at
  runtime. `.d.cts` files are now generated and the exports map carries
  per-condition types.
- The `rls-notes` demo was flaky in CI — PostgREST authenticates as a role
  pwrapd creates at startup, so a sidecar started first sat in a failed-auth
  retry loop that outlasted the demo.

### Known limitations

- `admin_audit_log` has no retention policy and grows without bound.
- Credential expiry bounds new connections, not open ones: Postgres checks
  credentials at authentication, so an established session survives its role's
  expiry. See [SECURITY.md](SECURITY.md).
- Revoking an API key does not immediately kill DSNs it already minted; those
  roles expire on their own schedule.
- SDK parity: TypeScript lacks `withUser`; neither TypeScript nor Python has a
  `matview` helper. Tracked in
  [`conformance/scenarios.json`](conformance/scenarios.json).

## [0.1.1] - 2026-09-14

Maintenance release. No API changes — the SDK surface is identical to 0.1.0.

### Changed

- Dependencies: pgx 5.9.1 -> 5.10.0, River 0.34.0 -> 0.42.0, OpenTelemetry
  1.41 -> 1.45, testcontainers 0.42 -> 0.44, and the GitHub Actions used by CI.
- `semconv` moved to v1.43.0 to match OpenTelemetry 1.45's default resource.
  Mismatched schema URLs make `resource.Merge` fail outright, so this had to move
  with otel rather than after it.
- TypeScript pinned to ^5.9.3. Versions 6 and 7 break `tsup`'s declaration
  output — the JS still emits, so the package would publish with no `.d.ts` at
  all. Revisit when tsup ships a rollup-plugin-dts built against TypeScript 7.

### Fixed

- The `rls-notes` end-to-end demo was flaky in CI, failing roughly four runs in
  ten. PostgREST authenticates as a role that pwrapd creates at startup, so a
  sidecar started first sat in a failed-auth retry loop that outlasted the demo.
  `scripts/e2e.sh` now restarts it once the role exists, which fixes the nightly
  and `make e2e-rls` alike.

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
- **Python realtime** — `await client.subscribe(table=..., user_id=...)` returns an
  async iterator of `ChangeEvent`, reconnecting with exponential backoff and
  stopping on an auth rejection. Resolves only after the server's hello frame, so
  a write issued immediately afterwards can't race ahead of the subscription.

### Fixed

- Restored `cmd/pwrapd`, `cmd/pwrap`, `sdk/go/pwrap` and `sdk/py/pwrap`, which
  unanchored `.gitignore` patterns had excluded from version control. The
  patterns were meant to ignore compiled binaries but matched directories at any
  depth.
- Corrected the Go module path to `github.com/b3vet/pwrap`; the previous path
  pointed at a repository that does not exist, so the SDK could not be fetched.
- **pwrapd never exited on SIGTERM.** The realtime hub holds a dedicated LISTEN
  connection and `pool.Close()` blocks until every connection is released, but
  the daemon never called `StopHub()` — so shutdown hung indefinitely and every
  restart or rolling deploy required a SIGKILL. `server.go` documented the
  ordering requirement; only the test harness honoured it.
- `sql apply` now notifies PostgREST to reload its schema cache. A table created
  through the escape hatch was invisible over REST — 404 on every request — until
  an unrelated project create or delete happened to trigger a reload.
- **TypeScript SDK**: `client.table()` is generic again, so
  `c.table<T>("name")` type-checks; `PwrapConfig.driver` accepts an async
  factory, without which the documented Neon edge-runtime setup could not
  compile; and `@neondatabase/serverless` is no longer bundled into
  `dist/neon.js` (215 KB → 388 B), restoring it to a genuinely optional
  dependency.
- **TypeScript SDK realtime on Node 18 and 20.** `subscribe()` called the global
  `WebSocket`, which Node only provides from v22, so it failed with
  "WebSocket is not defined" on the versions `engines` claimed to support. It
  now falls back to the optional `ws` package and, when neither is available,
  says so and names the fix.
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
  `withUser` (RLS scoping); neither TypeScript nor Python has a `matview`
  helper or the change-capture toggles. Each gap is recorded in
  [`conformance/scenarios.json`](conformance/scenarios.json) and printed on every
  conformance run.

[Unreleased]: https://github.com/b3vet/pwrap/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/b3vet/pwrap/releases/tag/v0.2.1
[0.2.0]: https://github.com/b3vet/pwrap/releases/tag/v0.2.0
[0.1.1]: https://github.com/b3vet/pwrap/releases/tag/v0.1.1
[0.1.0]: https://github.com/b3vet/pwrap/releases/tag/v0.1.0
