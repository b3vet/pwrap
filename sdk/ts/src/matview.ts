import type { Sql, SqlRef } from "./client.js";

/** Registry row for a matview, as stored in `pwrap_matviews`. */
export interface MatviewInfo {
  name: string;
  definition: string;
  checksum: string;
  last_refresh_at: Date | null;
  last_error: string | null;
  enabled: boolean;
  created_at: Date;
  updated_at: Date;
}

/**
 * Handle over a named materialized view backed by the `pwrap_matviews` registry.
 *
 * Scheduling is the caller's job — `refresh()` is the primitive to run on a cron,
 * a River periodic job, or on demand.
 */
export class Matview {
  constructor(private ref: SqlRef, private name: string) {}

  private run<R>(fn: (sql: Sql) => Promise<R>): Promise<R> {
    return this.ref.run(fn);
  }

  /**
   * Ensure the matview exists with this definition and record it in the registry.
   *
   * `definition` is the SQL *after* the `AS` — e.g.
   * `SELECT data->>'topic' AS topic, count(*) FROM pwrap_documents GROUP BY 1`.
   * If a matview is already registered under a different definition it is dropped
   * and recreated. The new matview is created `WITH NO DATA`; the first
   * `refresh()` populates it.
   */
  async register(definition: string): Promise<void> {
    validateIdent(this.name);
    const def = definition.trim();
    if (!def) throw new Error("pwrap: matview: definition is empty");
    const checksum = await sha256Hex(def);

    return this.run(async (sql) => {
      const existing = await sql<{ checksum: string }[]>`
        SELECT checksum FROM pwrap_matviews WHERE name = ${this.name}
      `;
      // A matching checksum means the view already reflects this definition;
      // re-creating it would throw away populated data for nothing.
      if (existing.length > 0 && existing[0].checksum === checksum) return;

      const quoted = quoteIdent(this.name);
      if (existing.length > 0) {
        await sql.unsafe(`DROP MATERIALIZED VIEW IF EXISTS ${quoted}`);
      }
      // DDL can't be parameterised, hence unsafe() — the name is validated above
      // and the definition is caller-supplied SQL by design.
      await sql.unsafe(`CREATE MATERIALIZED VIEW IF NOT EXISTS ${quoted} AS ${def} WITH NO DATA`);
      // Whoever runs CREATE owns the result, and clients connect as a credential
      // that expires within the hour. Left alone the matview outlives its owner:
      // REFRESH requires ownership, so the next credential — including this
      // client's own, after its hourly rotation — is refused with "must be
      // owner". The schema's owner is the project's tenant role, which outlives
      // every credential and which every ephemeral role inherits.
      const owner = await sql<{ owner: string }[]>`
        SELECT pg_get_userbyid(nspowner) AS owner FROM pg_namespace WHERE nspname = current_schema()
      `;
      await sql.unsafe(`ALTER MATERIALIZED VIEW ${quoted} OWNER TO ${quoteIdent(owner[0].owner)}`);
      // No unique index is synthesised: only the caller knows which columns are
      // unique, and REFRESH CONCURRENTLY needs one. See refreshConcurrent().
      await sql`
        INSERT INTO pwrap_matviews (name, definition, checksum)
        VALUES (${this.name}, ${def}, ${checksum})
        ON CONFLICT (name) DO UPDATE
            SET definition = EXCLUDED.definition,
                checksum   = EXCLUDED.checksum,
                updated_at = now()
      `;
    });
  }

  /** `REFRESH MATERIALIZED VIEW`. Takes an ACCESS EXCLUSIVE lock for the duration. */
  async refresh(): Promise<void> {
    return this.doRefresh(false);
  }

  /**
   * `REFRESH MATERIALIZED VIEW CONCURRENTLY` — no ACCESS EXCLUSIVE lock, so reads
   * continue during the refresh. Postgres requires the matview to carry a unique
   * index and to have been populated at least once, so call `refresh()` first and
   * create the index yourself.
   */
  async refreshConcurrent(): Promise<void> {
    return this.doRefresh(true);
  }

  private async doRefresh(concurrent: boolean): Promise<void> {
    validateIdent(this.name);
    const stmt = `REFRESH MATERIALIZED VIEW ${concurrent ? "CONCURRENTLY " : ""}${quoteIdent(this.name)}`;
    return this.run(async (sql) => {
      let failure: unknown;
      try {
        await sql.unsafe(stmt);
      } catch (e) {
        failure = e;
      }
      // The registry is updated either way: a success stamps last_refresh_at and
      // clears the error, a failure leaves the old timestamp and records why.
      const message = failure === undefined ? null : String((failure as Error)?.message ?? failure);
      await sql`
        UPDATE pwrap_matviews
           SET last_refresh_at = CASE WHEN ${message}::text IS NULL THEN now() ELSE last_refresh_at END,
               last_error      = ${message},
               updated_at      = now()
         WHERE name = ${this.name}
      `;
      if (failure !== undefined) {
        throw new Error(`pwrap: matview refresh: ${message}`);
      }
    });
  }

  /** Drop the matview and forget its registry row. */
  async drop(): Promise<void> {
    validateIdent(this.name);
    return this.run(async (sql) => {
      await sql.unsafe(`DROP MATERIALIZED VIEW IF EXISTS ${quoteIdent(this.name)}`);
      await sql`DELETE FROM pwrap_matviews WHERE name = ${this.name}`;
    });
  }

  /** The current registry row. Throws if the matview was never registered. */
  async info(): Promise<MatviewInfo> {
    return this.run(async (sql) => {
      const rows = await sql<MatviewInfo[]>`
        SELECT name, definition, checksum, last_refresh_at, last_error, enabled, created_at, updated_at
        FROM pwrap_matviews WHERE name = ${this.name}
      `;
      if (rows.length === 0) throw new Error(`pwrap: matview ${this.name} is not registered`);
      return rows[0];
    });
  }
}

// --- helpers -----------------------------------------------------------------

/**
 * Matview names are interpolated into DDL, which cannot be parameterised, so the
 * accepted alphabet is narrowed to what a plain Postgres identifier can hold.
 * Exotic quoted names are rejected rather than escaped.
 */
function validateIdent(s: string): void {
  if (s.length === 0 || s.length > 63) {
    throw new Error("pwrap: matview: name must be 1..63 chars");
  }
  const bad = s.search(/[^A-Za-z0-9_]/);
  if (bad !== -1) {
    throw new Error(`pwrap: matview: invalid char ${JSON.stringify(s[bad])} at pos ${bad}`);
  }
}

function quoteIdent(s: string): string {
  return '"' + s.replaceAll('"', '""') + '"';
}

/**
 * The checksum has to match the Go and Python SDKs byte for byte, or the three
 * would each think the other's registration was stale and drop the view.
 *
 * Global WebCrypto rather than `node:crypto`, so this works unchanged on the edge
 * and nothing Node-specific enters the bundle. Checked against node:18.0.0 — the
 * oldest release package.json admits — which already exposes `crypto.subtle`.
 */
async function sha256Hex(s: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s));
  return Array.from(new Uint8Array(digest))
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("");
}
