import postgres from "postgres";
import { Table } from "./table.js";
import { Vector } from "./vector.js";
import { Queue } from "./queue.js";
import { Geo } from "./geo.js";
import { issueRestToken, type RestToken, type RestTokenOpts } from "./rest.js";
import { Subscription, type SubscribeOpts } from "./subscribe.js";
import { SchemaVersion } from "./constants.js";

export interface PwrapConfig {
  /** pwrapd base URL. Defaults to `http://localhost:8080`. */
  controlUrl?: string;
  /** Project API key (`pwk_...`). Required. */
  apiKey: string;
  /**
   * Optional: custom fetch implementation. Defaults to global `fetch` (Node 18+).
   */
  fetch?: typeof fetch;
  /**
   * Optional driver factory. Defaults to `postgres` (postgres.js). Swap in
   * `@pwrap/sdk/neon`'s factory for Neon/edge. Must return an object compatible
   * with a minimal subset of postgres.js: tagged-template query + .unsafe() + .end().
   */
  driver?: (dsn: string) => Sql;
}

interface ConnectionResp {
  dsn: string;
  schema: string;
  schema_version: string;
  expires_at: string;
}

/** Minimal surface of postgres.js we depend on; swappable via PwrapConfig.driver. */
export type Sql = postgres.Sql<{}>;

export class PwrapClient {
  readonly schema: string;
  readonly expiresAt: Date;
  readonly sql: Sql;

  // Underscore-prefixed to signal "used by rest.ts but not part of the public API."
  readonly _controlUrl: string;
  readonly _apiKey: string;
  readonly _fetch: typeof fetch;

  private constructor(sql: Sql, schema: string, expiresAt: Date, controlUrl: string, apiKey: string, f: typeof fetch) {
    this.sql = sql;
    this.schema = schema;
    this.expiresAt = expiresAt;
    this._controlUrl = controlUrl;
    this._apiKey = apiKey;
    this._fetch = f;
  }

  /** Bootstrap: exchange the API key at `/v1/connection` and open a DB connection. */
  static async connect(cfg: PwrapConfig): Promise<PwrapClient> {
    if (!cfg.apiKey) throw new Error("pwrap: apiKey is required");
    const controlUrl = (cfg.controlUrl ?? "http://localhost:8080").replace(/\/+$/, "");
    const f = cfg.fetch ?? fetch;
    const resp = await f(`${controlUrl}/v1/connection`, {
      method: "POST",
      headers: { Authorization: `Bearer ${cfg.apiKey}` },
    });
    if (!resp.ok) {
      const text = await resp.text();
      throw new Error(`pwrap: /v1/connection ${resp.status} ${resp.statusText}: ${text}`);
    }
    const body = (await resp.json()) as ConnectionResp;

    if (!body.schema_version) {
      throw new Error(
        "pwrap: project has no migrations applied — run `pwrap migrate apply --project <id>`",
      );
    }
    if (body.schema_version < SchemaVersion) {
      throw new Error(
        `pwrap: tenant schema is "${body.schema_version}" but this SDK requires "${SchemaVersion}" — run \`pwrap migrate apply --project <id>\` to upgrade`,
      );
    }

    const driver = cfg.driver ?? ((dsn: string) => postgres(dsn, { prepare: false }));
    const sql = driver(body.dsn);
    return new PwrapClient(sql, body.schema, new Date(body.expires_at), controlUrl, cfg.apiKey, f);
  }

  /** Exchange the API key for a PostgREST JWT. */
  async issueRestToken(opts: RestTokenOpts = {}): Promise<RestToken> {
    return issueRestToken(this, opts);
  }

  /**
   * Open a WebSocket subscription to change events. Returns a Subscription that
   * is an AsyncIterable<ChangeEvent>. Awaits the initial hello before resolving
   * so the caller doesn't race the first event.
   */
  async subscribe(opts: SubscribeOpts = {}): Promise<Subscription> {
    const sub = new Subscription(this, opts);
    await sub.ready();
    return sub;
  }

  table(collection: string): Table {
    return new Table(this.sql, collection);
  }

  vector(collection: string): Vector {
    return new Vector(this.sql, collection);
  }

  queue(): Queue {
    return new Queue(this.sql);
  }

  geo(collection: string): Geo {
    return new Geo(this.sql, collection);
  }

  async close(): Promise<void> {
    await this.sql.end({ timeout: 5 });
  }
}
