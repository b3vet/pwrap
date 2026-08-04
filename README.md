# pwrap

An opinionated, all-in-one Postgres backend. One tool that gives a new project typed tables, JSONB documents, a durable job queue, vector search, migrations, and API-key auth on day one — backed by plain Postgres (or Neon).

**Status:** pre-alpha. Public API will change.

## What's in the box

**MVP (M1–M6):**
- **JSONB document tables** with GIN indexes, over a single `pwrap_documents` table
- **Durable queue** (`SELECT … FOR UPDATE SKIP LOCKED`) via [River](https://github.com/riverqueue/river) — retries, DLQ, visibility
- **Vector search** via [pgvector](https://github.com/pgvector/pgvector) + HNSW (1536-dim)
- **Migrations** applied per-project (baseline + River schema)
- **Control plane** (`pwrapd`): projects, argon2id API keys, scoped DSN handoff
- **SDKs**: Go and TypeScript

**Track A (M7–M9):**
- **Auto REST + GraphQL** per project via [PostgREST](https://postgrest.org) + [pg_graphql](https://github.com/supabase/pg_graphql). `/v1/rest/token` issues a JWT; hit PostgREST directly.
- **Row-level security helpers** — `Client.WithUser(id)` wraps queries in a tx with `SET LOCAL request.jwt.claims`, so one RLS policy enforces isolation over both the SDK and PostgREST.
- **`pwrap sql apply --file …`** escape hatch for user-defined tables.
- **Materialized views** — `Client.Matview(name).Register/Refresh`, with a `pwrap_matviews` registry tracking `last_refresh_at`.

**Track B (M10–M12):**
- **PostGIS Geo collections** — `Client.Geo(coll).InsertPoint/Insert/WithinRadius/WithinBBox/Nearest` over a `pwrap_geo` table with GIST (KNN + bbox).
- **BRIN indexes** baked into the baseline on `created_at` of every built-in table (`pwrap_documents`, `pwrap_embeddings`, `pwrap_geo`) — orders of magnitude smaller than B-tree, fast enough for time-range scans on append-mostly data.
- **Declarative partitioning helper** — `SELECT pwrap_partition_ensure('events', now())` materialises a monthly partition idempotently. Schedule from a River periodic job or pg_cron.

**Track C (M13–M15):**
- **Project branching** — `POST /v1/projects/:id/branches` creates a child project (fresh role + schema + baseline). `with_data: true` snapshots the parent's `pwrap_documents` / `embeddings` / `geo` / `matviews`. Post-fork edits are isolated; `pwrap branch sync` does idempotent merges.
- **Python SDK** — `pip install pwrap`. Async via `asyncpg` + `httpx` + `websockets`. Mirrors Go/TS: `Table`, `Vector`, `Queue`, `Geo`, `subscribe(...)` for realtime, `with_user(...)` for RLS, `issue_rest_token(...)`.
- **Neon integration** — `pwrap neon branch create/list/delete` + `pwrap neon connection-uri` wrap the Neon Cloud API for per-PR previews. `PWRAP_NEON_BASE_URL` overrides for self-hosted Neon. See [docs/neon-integration.md](docs/neon-integration.md).

**Path P (M23–M24) — Productionization:**
- **Encryption at rest** — project `pg_password` is encrypted with AES-256-GCM via an envelope (`v1:<base64>`) keyed by `PWRAP_ENCRYPTION_KEY`. pwrapd re-encrypts any legacy plaintext rows on startup; unset key falls back to cleartext with a loud warning. See [internal/crypto](internal/crypto).
- **OpenTelemetry** — tracing (HTTP via `otelhttp`, pgx via a built-in `QueryTracer`) and metrics export over OTLP/HTTP. A Prometheus `/metrics` endpoint is available out of the box. Configure via `PWRAP_OTLP_ENDPOINT`, `PWRAP_METRICS_ADDR`, `PWRAP_TRACE_SAMPLE_RATE`.

**Path R (M19–M22) — Realtime:**
- **Change capture** — `pwrap_documents` writes are auto-captured by a trigger that records to `pwrap_change_log` and emits `pg_notify('pwrap_changes', …)`. `Client.EnableChangeCapture(ctx, "<table>")` opts a user table in.
- **WebSocket subscriptions** — `GET /v1/subscribe?api_key=…&table=…&user_id=…` upgrades to WebSocket and pushes filtered events. `Client.Subscribe(ctx, …)` in Go, `client.subscribe(...)` (`AsyncIterable<ChangeEvent>`) in TS, `await c.subscribe(...)` in Python.
- **Live demo** — [examples/realtime-todos](examples/realtime-todos): a Go web app + embedded HTML/JS that subscribes from the browser directly. Open the page in two tabs and watch inserts/updates/deletes sync live with zero polling.

## Architecture

Hybrid: SDKs talk to Postgres directly on the hot path; a thin control plane owns projects / keys / migrations + issues PostgREST JWTs.

```
[SDK]    --(api key)--> [pwrapd] --(scoped DSN)---> [Postgres]
[SDK]    ---------- pgx / postgres.js ------------> [Postgres]
[App/cURL] --(JWT)----> [PostgREST] --(SET ROLE)--> [Postgres]
[App]    --(api key)--> [pwrapd /v1/rest/token] --> JWT for PostgREST
```

On project create, `pwrapd` provisions a Postgres role + schema (`p_<slug>`). On bootstrap, the SDK exchanges its API key for a scoped DSN and caches it until expiry.

> **Credential lifetime.** The DSN returned by `/v1/connection` carries the tenant role's *stable* password, and its `expires_at` (24h) is a refresh hint for the SDK — nothing enforces it server-side. An issued DSN therefore keeps working past `expires_at`, and **revoking the API key does not revoke a DSN already handed out.** Rotating per-issue credentials are on the roadmap; until then, treat an issued DSN as a long-lived secret. See [SECURITY.md](SECURITY.md).

## Quickstart

```bash
# 1. Local Postgres (pgvector image)
docker-compose up -d postgres

# 2. Build
make build

# 3. Start pwrapd (in one shell)
export PWRAP_DATABASE_URL="postgres://pwrap:pwrap@localhost:5432/pwrap?sslmode=disable"
export PWRAP_BOOTSTRAP_TOKEN="dev-admin"
./bin/pwrapd

# 4. Poke it (in another shell)
./bin/pwrap project create --name hello
./bin/pwrap migrate apply --project <id>
./bin/pwrap key issue --project <id> --name my-app
```

Or run the demos in one shot:

```bash
cp .env.example .env  # PostgREST needs the auth password baked into its container

# MVP dogfood: CRUD + queue + vector + matview refresh
make e2e

# Track A: per-user RLS proven identically via SDK and PostgREST
make e2e-rls

# Track B: PostGIS radius/bbox/nearest + declarative partitioning
make e2e-geo

# Track C: project branching with data copy + parent/child isolation
make e2e-branching

# Track C: Python SDK pytest smoke (requires a live pwrapd)
make py-test

# Path R: realtime web demo on http://localhost:7799 (open in two tabs)
make e2e-realtime
```

See [`examples/`](examples) for the code — `todo-plus`, `rls-notes`, `geo-spots`, `branching`, `python-demo`, `bench`.

## PostgREST + JWT

`pwrapd` runs PostgREST as a sidecar. To use the auto REST/GraphQL API, exchange your project API key for a short-lived JWT:

```bash
curl -X POST -H "Authorization: Bearer pwk_..." http://localhost:8080/v1/rest/token
# → {"token":"eyJhbGc...","url":"http://localhost:3000","role":"p_myproj","expires_at":"..."}

# then hit PostgREST directly
curl -H "Authorization: Bearer <jwt>" -H "Accept-Profile: p_myproj" http://localhost:3000/todos

# GraphQL at /rpc/graphql
curl -X POST -H "Authorization: Bearer <jwt>" -H "Content-Profile: p_myproj" \
    -d '{"query":"{ todosCollection(first: 5) { edges { node { id title } } } }"}' \
    http://localhost:3000/rpc/graphql
```

Real tables (beyond pwrap_documents) are created via the escape-hatch CLI:

```bash
pwrap sql apply --project <id> --file schema.sql
```

Required env for REST: `PWRAP_AUTHENTICATOR_PASSWORD`, `PWRAP_JWT_SECRET`.

## Go SDK

```go
import "github.com/b3vet/pwrap/sdk/go/pwrap"

c, _ := pwrap.New(ctx, pwrap.Config{
    ControlURL: "http://localhost:8080",
    APIKey:     "pwk_...",
})
defer c.Close()

// JSONB collection
notes := c.Table("notes")
id, _ := notes.Insert(ctx, map[string]any{"title": "hello", "tags": []string{"a"}})
docs, _ := notes.Find(ctx, map[string]any{"tags": []string{"a"}}, 10)

// pgvector
v := c.Vector("notes")
_ = v.Upsert(ctx, id.String(), embedding /* []float32, dim=1536 */, nil)
matches, _ := v.Search(ctx, query, 5)

// Queue (insert-only; register workers with River against c.Pool())
_, _ = c.Queue().Enqueue(ctx, pwrap.EnqueueRequest{Kind: "embed", Args: map[string]any{"id": id}})

// PostGIS geo collection
geo := c.Geo("places")
_, _ = geo.InsertPoint(ctx, 2.2945, 48.8584, map[string]any{"name": "Eiffel Tower"})
near, _ := geo.WithinRadius(ctx, 2.3522, 48.8566, 2000 /* meters */, 10)
nearest, _ := geo.Nearest(ctx, 13.405, 52.520, 5)

// RLS-scoped client — Table ops run inside a tx with request.jwt.claims.user_id set
alice := c.WithUser("alice-uuid")
_, _ = alice.Table("notes").Insert(ctx, map[string]any{"user_id": "alice-uuid", "title": "private"})

// Materialised views
mv := c.Matview("notes_word_counts")
_ = mv.Register(ctx, `SELECT split_part(data->>'title', ' ', 1) AS w, count(*) FROM pwrap_documents GROUP BY 1`)
_ = mv.Refresh(ctx)
```

## Python SDK

```python
import asyncio
from pwrap import PwrapClient

async def main() -> None:
    async with await PwrapClient.connect(api_key="pwk_...") as c:
        notes = c.table("notes")
        doc_id = await notes.insert({"title": "hello", "tags": ["a"]})
        docs  = await notes.find({"tags": ["a"]})

        # RLS-scoped, identical contract to Go SDK's WithUser
        alice = c.with_user("alice")
        await alice.table("notes").insert({"user_id": "alice", "title": "private"})

        # PostgREST JWT
        tok = await c.issue_rest_token(user_id="alice")
        print(tok.token)

        # Realtime — resolves after the server's hello frame, so the next
        # write can't race ahead of the subscription
        sub = await c.subscribe(table="pwrap_documents")
        async for ev in sub:
            print(ev.op, ev.row_id)
            break
        await sub.close()

asyncio.run(main())
```

Install: `pip install pwrap` (or from the workspace: `pip install -e sdk/py`).

## TypeScript SDK

```ts
import { PwrapClient } from "@pwrap/sdk";

const c = await PwrapClient.connect({
  controlUrl: "http://localhost:8080",
  apiKey: "pwk_...",
});

const notes = c.table<{ title: string; tags: string[] }>("notes");
const id = await notes.insert({ title: "hello", tags: ["a"] });

await c.vector("notes").upsert(id, embedding /* number[1536] */);
await c.queue().enqueue({ kind: "embed", args: { id } });

await c.close();
```

For edge runtimes (Cloudflare Workers, Vercel Edge) where TCP isn't available,
swap in Neon's serverless driver:

```ts
import { PwrapClient } from "@pwrap/sdk";
import { neonDriverAsync } from "@pwrap/sdk/neon";

const c = await PwrapClient.connect({
  apiKey: "pwk_...",
  driver: neonDriverAsync,
});
```

`@neondatabase/serverless` is an optional peer dependency — install it only if you use this path.

## Roadmap

- **Rotating DSN credentials** — make `/v1/connection` genuinely short-lived, with server-side expiry and revocation. See the credential note above.
- **SDK parity** — `withUser` and `matview` in TypeScript; `subscribe` and `matview` in Python.
- **Scoped admin credentials** — replace the single `PWRAP_BOOTSTRAP_TOKEN` with scoped tokens and an audit trail.
- **SDK DSN auto-refresh** — re-exchange the API key on expiry instead of caching until it fails.
- **Declarative typed-schema API** — define tables in the SDK instead of via `sql apply`.
- **Hosted SaaS console.**

## Development

```bash
make help       # list every target
make test       # unit tests
make lint       # golangci-lint v2
make e2e        # end-to-end dogfood
make test-integration        # testcontainers suite (needs Docker)
cd sdk/ts && pnpm run build  # build TS SDK
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for repository layout, expectations, and where help is most useful.

## Project

- [CHANGELOG.md](CHANGELOG.md) — release notes and known limitations
- [SECURITY.md](SECURITY.md) — trust model, production checklist, how to report a vulnerability
- [CONTRIBUTING.md](CONTRIBUTING.md) — development setup and contribution guide
- [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## License

Apache 2.0 — see [LICENSE](LICENSE).
