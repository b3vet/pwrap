// Cross-SDK conformance runner for the TypeScript SDK. Runs the scenarios
// listed for "ts" in conformance/scenarios.json against a live pwrapd and writes
// report-ts.json, which conformance/check.py then gates on.
//
//   PWRAP_CONTROL_URL      default http://localhost:8080
//   PWRAP_BOOTSTRAP_TOKEN  default dev-admin
//   PWRAP_REPORT_DIR       where to write report-ts.json (default: cwd)
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { randomUUID } from "node:crypto";

import { PwrapClient, VectorDim } from "@pwrap/sdk";

const CONTROL_URL = process.env.PWRAP_CONTROL_URL ?? "http://localhost:8080";
const ADMIN_TOKEN = process.env.PWRAP_BOOTSTRAP_TOKEN ?? "dev-admin";
const REPORT_DIR = process.env.PWRAP_REPORT_DIR ?? ".";

// Mirrors the Go and Python runners exactly, so all three are compared on
// identical vectors rather than merely similar ones.
function fakeEmbedding(seed: number): number[] {
  return Array.from({ length: VectorDim }, (_, i) => Math.sin(seed + i * 0.001));
}

function assert(cond: unknown, msg: string): asserts cond {
  if (!cond) throw new Error(msg);
}

// --- control plane -----------------------------------------------------------

async function admin(method: string, path: string, body?: unknown): Promise<any> {
  const res = await fetch(CONTROL_URL + path, {
    method,
    headers: {
      Authorization: `Bearer ${ADMIN_TOKEN}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${await res.text()}`);
  if (res.status === 204) return null;
  const text = await res.text();
  return text ? JSON.parse(text) : null;
}

async function createProject(name: string): Promise<string> {
  // Unique suffix so reruns against a live stack don't 409.
  const p = await admin("POST", "/v1/projects", { name: `${name}-${randomUUID().slice(0, 8)}` });
  return p.id;
}

async function provision(name: string): Promise<{ projectId: string; apiKey: string }> {
  const projectId = await createProject(name);
  await admin("POST", `/v1/projects/${projectId}/migrations`);
  const k = await admin("POST", `/v1/projects/${projectId}/keys`, { name: "conformance" });
  return { projectId, apiKey: k.key };
}

async function withClient<T>(name: string, fn: (c: PwrapClient, projectId: string) => Promise<T>): Promise<T> {
  const { projectId, apiKey } = await provision(name);
  const c = await PwrapClient.connect({ controlUrl: CONTROL_URL, apiKey });
  try {
    return await fn(c, projectId);
  } finally {
    await c.close().catch(() => {});
    await admin("DELETE", `/v1/projects/${projectId}`).catch(() => {});
  }
}

// --- scenarios ----------------------------------------------------------------

const scenarios: Record<string, () => Promise<void>> = {
  table_crud: () =>
    withClient("conf-table-crud", async (c) => {
      const notes = c.table<{ title: string; tags: string[]; pinned?: boolean }>("notes");
      const id = await notes.insert({ title: "hello", tags: ["a"] });

      const got = await notes.get(id);
      assert(got?.data.title === "hello", `title = ${got?.data.title}, want hello`);

      const rows = await notes.find({ tags: ["a"] });
      assert(rows.length === 1, `find returned ${rows.length} rows, want 1`);

      await notes.update(id, { pinned: true });
      const got2 = await notes.get(id);
      assert(got2?.data.pinned === true, `pinned = ${got2?.data.pinned}, want true`);

      const n = await notes.count({ title: "hello" });
      assert(n === 1, `count = ${n}, want 1`);

      await notes.delete(id);
      assert((await notes.get(id)) === null, "get after delete returned a row");
      // Deleting a second time must not silently succeed.
      let threw = false;
      try {
        const ok = await notes.delete(id);
        threw = ok === false;
      } catch {
        threw = true;
      }
      assert(threw, "second delete reported success, want failure");
    }),

  table_batch: () =>
    withClient("conf-table-batch", async (c) => {
      const notes = c.table<{ title: string }>("notes");
      const ids = await notes.insertMany([{ title: "a" }, { title: "b" }, { title: "c" }]);
      assert(ids.length === 3, `got ${ids.length} ids, want 3`);
      // Order must match the input, not insertion race order.
      const titles = ["a", "b", "c"];
      for (let i = 0; i < ids.length; i++) {
        const doc = await notes.get(ids[i]);
        assert(
          doc?.data.title === titles[i],
          `ids[${i}] has title ${doc?.data.title}, want ${titles[i]} — order not preserved`,
        );
      }
    }),

  vector_upsert_search: () =>
    withClient("conf-vector", async (c) => {
      const v = c.vector("notes");
      await v.upsert("a", fakeEmbedding(0), { name: "a" });
      await v.upsert("b", fakeEmbedding(10), { name: "b" });
      await v.upsert("c", fakeEmbedding(20), { name: "c" });

      const matches = await v.search(fakeEmbedding(0.01), 3);
      assert(matches.length === 3, `got ${matches.length} matches, want 3`);
      assert(matches[0].doc_id === "a", `nearest = ${matches[0].doc_id}, want a`);
      for (let i = 1; i < matches.length; i++) {
        assert(
          matches[i].distance >= matches[i - 1].distance,
          `distances not ascending: ${matches[i - 1].distance} then ${matches[i].distance}`,
        );
      }
    }),

  vector_dim_validation: () =>
    withClient("conf-vector-dim", async (c) => {
      let threw = false;
      try {
        await c.vector("notes").upsert("short", [0.1, 0.2]);
      } catch {
        threw = true;
      }
      assert(threw, "upsert with 2 dimensions succeeded, want a validation error");
    }),

  queue_enqueue_stats: () =>
    withClient("conf-queue", async (c) => {
      const q = c.queue();
      const id = await q.enqueue({ kind: "embed", args: { doc_id: "x" } });
      assert(id > 0n || Number(id) > 0, `job id = ${id}, want positive`);
      const stats = await q.stats();
      assert(Number(stats.available) >= 1, `available = ${stats.available}, want >= 1`);
    }),

  geo_radius_nearest: () =>
    withClient("conf-geo", async (c) => {
      const geo = c.geo("places");
      await geo.insertPoint(2.2945, 48.8584, { name: "Eiffel" });
      await geo.insertPoint(2.3499, 48.853, { name: "Notre-Dame" });
      await geo.insertPoint(-73.9857, 40.7484, { name: "Empire State" });

      const near = await geo.withinRadius(2.3522, 48.8566, 5000, 100);
      const names = near.map((f) => String(f.metadata?.name)).sort();
      assert(
        names.join(",") === "Eiffel,Notre-Dame",
        `within 5km of Paris = ${JSON.stringify(names)}, want [Eiffel, Notre-Dame]`,
      );

      const nearest = await geo.nearest(2.3522, 48.8566, 1);
      assert(nearest.length === 1, `nearest returned ${nearest.length}, want 1`);
      const n = String(nearest[0].metadata?.name);
      assert(n === "Eiffel" || n === "Notre-Dame", `nearest to Paris = ${n}, want a Paris landmark`);
    }),

  rest_token: () =>
    withClient("conf-rest-token", async (c) => {
      const tok = await c.issueRestToken({ userId: "someone", ttlSeconds: 60 });
      assert(tok.token.startsWith("ey"), `token ${tok.token} does not look like a JWT`);
      assert(!!tok.url, "empty url");
      assert(tok.role.startsWith("p_"), `role = ${tok.role}, want a p_ tenant role`);
    }),

  schema_version_guard: async () => {
    // Deliberately skip `migrate apply` — connecting must fail loudly.
    const projectId = await createProject("conf-unmigrated");
    try {
      const k = await admin("POST", `/v1/projects/${projectId}/keys`, { name: "conformance" });
      let err: unknown;
      try {
        const c = await PwrapClient.connect({ controlUrl: CONTROL_URL, apiKey: k.key });
        await c.close();
      } catch (e) {
        err = e;
      }
      assert(err !== undefined, "connected to an unmigrated project, want an error");
      assert(
        String((err as Error).message).toLowerCase().includes("migrat"),
        `error ${(err as Error).message} does not mention migrations`,
      );
    } finally {
      await admin("DELETE", `/v1/projects/${projectId}`).catch(() => {});
    }
  },

  realtime_subscribe: () =>
    withClient("conf-realtime", async (c) => {
      const sub = await c.subscribe({ table: "pwrap_documents" });
      try {
        await c.table("live").insert({ marker: "realtime" });

        const event = await Promise.race([
          (async () => {
            for await (const ev of sub) return ev;
            return null;
          })(),
          new Promise<null>((r) => setTimeout(() => r(null), 30_000)),
        ]);
        assert(event !== null, "timed out waiting for the change event");
        assert(
          String(event.op).toUpperCase() === "INSERT",
          `op = ${event.op}, want INSERT`,
        );
      } finally {
        sub.close();
      }
    }),
};

// --- main ---------------------------------------------------------------------

const results: Record<string, string> = {};
let failed = 0;

for (const [id, run] of Object.entries(scenarios)) {
  try {
    await run();
    results[id] = "pass";
    console.log(`ok   ${id}`);
  } catch (e) {
    results[id] = `fail: ${(e as Error).message}`;
    console.log(`FAIL ${id}: ${(e as Error).message}`);
    failed++;
  }
}

writeFileSync(
  join(REPORT_DIR, "report-ts.json"),
  JSON.stringify({ sdk: "ts", results }, null, 2) + "\n",
);

const total = Object.keys(scenarios).length;
console.log(`\nts: ${total - failed}/${total} scenarios passed`);
process.exit(failed > 0 ? 1 : 0);
