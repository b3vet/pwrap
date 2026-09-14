import type { Sql, SqlRef } from "./client.js";

export interface EnqueueRequest {
  kind: string;
  args?: Record<string, unknown>;
  queue?: string;
  /** 1 (highest) .. 4 (lowest). Default 1. */
  priority?: number;
  /** Default 25 (River's default). */
  maxAttempts?: number;
}

export interface Stats {
  available: number;
  running: number;
  scheduled: number;
  completed: number;
  discarded: number;
  retryable: number;
}

/**
 * Queue inserts directly into `river_job`. Workers are Go-only at MVP; use the Go SDK
 * (or the River library directly) to register workers against the same tenant schema.
 */
export class Queue {
  constructor(private ref: SqlRef) {}

  // Everything goes through the ref rather than a captured Sql: the client
  // swaps its connection when credentials near expiry, and a user-scoped ref
  // additionally wraps each operation in a claims-carrying transaction.
  private run<R>(fn: (sql: Sql) => Promise<R>): Promise<R> {
    return this.ref.run(fn);
  }

  async enqueue(req: EnqueueRequest): Promise<bigint> {
    if (!req.kind) throw new Error("pwrap: queue: kind is required");
    const args = req.args ?? {};
    const queue = req.queue ?? "default";
    const priority = req.priority ?? 1;
    const maxAttempts = req.maxAttempts ?? 25;
    const rows = await this.run((sql) => sql<{ id: bigint }[]>`
      INSERT INTO river_job (kind, args, queue, priority, max_attempts)
      VALUES (${req.kind},
              ${
                // eslint-disable-next-line @typescript-eslint/no-explicit-any
                sql.json(args as any)
              },
              ${queue}, ${priority}, ${maxAttempts})
      RETURNING id
    `);
    return rows[0].id;
  }

  async stats(): Promise<Stats> {
    const rows = await this.run((sql) => sql<{ state: string; count: bigint }[]>`
      SELECT state::text, COUNT(*) FROM river_job GROUP BY state
    `);
    const out: Stats = { available: 0, running: 0, scheduled: 0, completed: 0, discarded: 0, retryable: 0 };
    for (const r of rows) {
      const key = r.state as keyof Stats;
      if (key in out) out[key] = Number(r.count);
    }
    return out;
  }
}
