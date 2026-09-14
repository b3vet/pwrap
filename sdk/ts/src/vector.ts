import type { Sql, SqlRef } from "./client.js";
import { VectorDim } from "./constants.js";

export interface Match {
  doc_id: string;
  metadata: Record<string, unknown>;
  distance: number;
}

export class Vector {
  constructor(private ref: SqlRef, private collection: string) {}

  // Everything goes through the ref rather than a captured Sql: the client
  // swaps its connection when credentials near expiry, and a user-scoped ref
  // additionally wraps each operation in a claims-carrying transaction.
  private run<R>(fn: (sql: Sql) => Promise<R>): Promise<R> {
    return this.ref.run(fn);
  }

  /**
   * Bulk version of upsert. All embeddings must be VectorDim long; mismatched
   * input throws synchronously before touching the DB. Encoded as a single jsonb
   * batch param for postgres.js compatibility.
   */
  async upsertMany(records: { docId: string; embedding: number[]; metadata?: Record<string, unknown> }[]): Promise<void> {
    if (records.length === 0) return;
    const payload: { doc_id: string; embedding: string; metadata: Record<string, unknown> }[] = [];
    for (let i = 0; i < records.length; i++) {
      const r = records[i];
      if (r.embedding.length !== VectorDim) {
        throw new Error(`pwrap: vector: record ${i}: expected dim ${VectorDim}, got ${r.embedding.length}`);
      }
      payload.push({
        doc_id: r.docId,
        embedding: "[" + r.embedding.join(",") + "]",
        metadata: r.metadata ?? {},
      });
    }
    await this.run(async (sql) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const json = sql.json(payload as any);
      await sql`
        INSERT INTO pwrap_embeddings (collection, doc_id, embedding, metadata)
        SELECT ${this.collection},
               rec->>'doc_id',
               (rec->>'embedding')::vector,
               rec->'metadata'
          FROM jsonb_array_elements(${json}::jsonb) AS rec
            ON CONFLICT (collection, doc_id) DO UPDATE
               SET embedding  = EXCLUDED.embedding,
                   metadata   = EXCLUDED.metadata,
                   created_at = now()
      `;
    });
  }

  async upsert(docId: string, embedding: number[], metadata: Record<string, unknown> = {}): Promise<void> {
    if (embedding.length !== VectorDim) {
      throw new Error(`pwrap: vector: expected dim ${VectorDim}, got ${embedding.length}`);
    }
    const lit = formatVector(embedding);
    await this.run(async (sql) => {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const meta = sql.json(metadata as any);
      await sql`
        INSERT INTO pwrap_embeddings (collection, doc_id, embedding, metadata)
        VALUES (${this.collection}, ${docId}, ${lit}::vector, ${meta})
        ON CONFLICT (collection, doc_id) DO UPDATE
           SET embedding = EXCLUDED.embedding,
               metadata  = EXCLUDED.metadata,
               created_at = now()
      `;
    });
  }

  async delete(docId: string): Promise<boolean> {
    return this.run(async (sql) => {
      const res = await sql`
        DELETE FROM pwrap_embeddings
        WHERE collection = ${this.collection} AND doc_id = ${docId}
      `;
      return res.count > 0;
    });
  }

  async search(query: number[], k = 10): Promise<Match[]> {
    if (query.length !== VectorDim) {
      throw new Error(`pwrap: vector: expected dim ${VectorDim}, got ${query.length}`);
    }
    const lit = formatVector(query);
    const rows = await this.run((sql) => sql<Match[]>`
      SELECT doc_id,
             metadata,
             embedding <=> ${lit}::vector AS distance
      FROM pwrap_embeddings
      WHERE collection = ${this.collection}
      ORDER BY embedding <=> ${lit}::vector
      LIMIT ${k}
    `);
    return rows.map((r) => ({ ...r, distance: Number(r.distance) }));
  }
}

function formatVector(v: number[]): string {
  return "[" + v.join(",") + "]";
}
