# @pwrap/sdk — TypeScript SDK

TypeScript client for [pwrap](https://github.com/b3vet/pwrap), a Postgres-backed
all-in-one backend.

The SDK exchanges a project API key for a scoped Postgres DSN at startup, then
talks to Postgres directly — the control plane is not in the query path.

## Install

```bash
npm install @pwrap/sdk      # or pnpm add / yarn add
```

## Usage

```ts
import { PwrapClient } from "@pwrap/sdk";

const c = await PwrapClient.connect({
  controlUrl: "http://localhost:8080",
  apiKey: "pwk_...",
});

// JSONB collections
const notes = c.table<{ title: string; tags: string[] }>("notes");
const id = await notes.insert({ title: "hello", tags: ["a"] });
const docs = await notes.find({ tags: ["a"] });

// pgvector search
await c.vector("notes").upsert(id, embedding /* number[1536] */);
const matches = await c.vector("notes").search(query, 5);

// Durable queue (insert-only; workers run against River in Go)
await c.queue().enqueue({ kind: "embed", args: { id } });

// PostGIS
await c.geo("places").insertPoint(2.2945, 48.8584, { name: "Eiffel Tower" });
const near = await c.geo("places").withinRadius(2.3522, 48.8566, 2000, 10);

// Realtime — subscribe() resolves to an AsyncIterable of change events
for await (const ev of await c.subscribe({ table: "pwrap_documents" })) {
  console.log(ev.op, ev.after);
}

await c.close();
```

### PostgREST tokens

```ts
const tok = await c.issueRestToken({ userId: "alice" });
// tok.token is an HS256 JWT; hit tok.url directly
```

### Edge runtimes

The default driver is [postgres.js](https://github.com/porsager/postgres). For
Neon's serverless stack, pass the Neon driver instead:

```ts
import { PwrapClient } from "@pwrap/sdk";
import { neonDriverAsync } from "@pwrap/sdk/neon";

const c = await PwrapClient.connect({
  apiKey: "pwk_...",
  driver: await neonDriverAsync(),
});
```

`@neondatabase/serverless` is an optional peer dependency — install it only if
you use this path.

## Status

Pre-alpha. The public API will change. See the
[main README](https://github.com/b3vet/pwrap#readme) for the full picture, and
[SECURITY.md](https://github.com/b3vet/pwrap/blob/main/SECURITY.md) for the
trust model before deploying.

## License

Apache 2.0.
