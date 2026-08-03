import type { Sql } from "./client.js";

export interface Document<T = Record<string, unknown>> {
  id: string;
  data: T;
  created_at: Date;
  updated_at: Date;
}

// postgres.js's sql.json() insists its input matches its internal JSONValue type,
// which excludes `unknown`. Our public API documents that data must be JSON-serializable;
// we cast through `any` to hand the value off without forcing users to carry the
// driver's internal type into their code.
type JSONLike = any; // eslint-disable-line @typescript-eslint/no-explicit-any

/** JSONB document collection backed by pwrap_documents + GIN. */
export class Table<T extends Record<string, unknown> = Record<string, unknown>> {
  constructor(private sql: Sql, private collection: string) {}

  async insert(data: T): Promise<string> {
    const rows = await this.sql<{ id: string }[]>`
      INSERT INTO pwrap_documents (collection, data)
      VALUES (${this.collection}, ${this.sql.json(data as JSONLike)})
      RETURNING id
    `;
    return rows[0].id;
  }

  /**
   * Bulk-insert a batch of documents in a single round-trip. Returns the generated
   * ids in input order. The whole batch travels as a single jsonb parameter; the
   * server unnests it via `jsonb_array_elements`. Avoids postgres.js's text[]
   * array-literal escaping issues with embedded JSON.
   */
  async insertMany(items: T[]): Promise<string[]> {
    if (items.length === 0) return [];
    const payload = this.sql.json(items as JSONLike);
    const rows = await this.sql<{ id: string }[]>`
      INSERT INTO pwrap_documents (collection, data)
      SELECT ${this.collection}, value
        FROM jsonb_array_elements(${payload}::jsonb) WITH ORDINALITY AS u(value, ord)
       ORDER BY ord
      RETURNING id
    `;
    return rows.map((r) => r.id);
  }

  async get(id: string): Promise<Document<T> | null> {
    const rows = await this.sql<Document<T>[]>`
      SELECT id, data, created_at, updated_at
      FROM pwrap_documents
      WHERE collection = ${this.collection} AND id = ${id}
    `;
    return rows[0] ?? null;
  }

  /** JSONB containment. Pass an object subset; rows whose data contains it match. */
  async find(filter: Partial<T> = {} as Partial<T>, limit = 100): Promise<Document<T>[]> {
    return this.sql<Document<T>[]>`
      SELECT id, data, created_at, updated_at
      FROM pwrap_documents
      WHERE collection = ${this.collection}
        AND data @> ${this.sql.json(filter as JSONLike)}
      ORDER BY created_at DESC
      LIMIT ${limit}
    `;
  }

  async update(id: string, patch: Partial<T>): Promise<boolean> {
    const res = await this.sql`
      UPDATE pwrap_documents
         SET data = data || ${this.sql.json(patch as JSONLike)},
             updated_at = now()
       WHERE collection = ${this.collection} AND id = ${id}
    `;
    return res.count > 0;
  }

  async delete(id: string): Promise<boolean> {
    const res = await this.sql`
      DELETE FROM pwrap_documents WHERE collection = ${this.collection} AND id = ${id}
    `;
    return res.count > 0;
  }

  async count(filter: Partial<T> = {} as Partial<T>): Promise<number> {
    const rows = await this.sql<{ count: bigint }[]>`
      SELECT COUNT(*) FROM pwrap_documents
      WHERE collection = ${this.collection}
        AND data @> ${this.sql.json(filter as JSONLike)}
    `;
    return Number(rows[0].count);
  }
}
