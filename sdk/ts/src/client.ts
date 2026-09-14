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
   *
   * May be async: the Neon factory dynamically imports its optional dependency,
   * so it can only resolve a driver on a promise.
   */
  driver?: (dsn: string) => Sql | Promise<Sql>;
}

interface ConnectionResp {
  dsn: string;
  schema: string;
  schema_version: string;
  expires_at: string;
}

/** Minimal surface of postgres.js we depend on; swappable via PwrapConfig.driver. */
export type Sql = postgres.Sql<{}>;

/**
 * Mutable holder for the live connection.
 *
 * Credentials are minted per exchange and expire server-side, so the client
 * replaces its connection before that happens. Handles read through this holder
 * rather than capturing an Sql, so one kept across a refresh keeps working.
 */
export interface SqlRef {
  current: Sql;
}

/** Re-exchange this long before expiry — enough to cover the call and reconnect. */
const REFRESH_LEAD_MS = 2 * 60 * 1000;
/** Floor, so a skewed clock or an already-stale expiry cannot spin the timer. */
const MIN_REFRESH_DELAY_MS = 30 * 1000;
/** Let in-flight queries drain from the old connection before ending it. */
const OLD_SQL_GRACE_MS = 5 * 1000;

export class PwrapClient {
  readonly schema: string;
  expiresAt: Date;
  private readonly ref: SqlRef;
  private refreshTimer?: ReturnType<typeof setTimeout>;
  private closed = false;

  /** The live connection. Replaced on refresh — read it, don't cache it. */
  get sql(): Sql {
    return this.ref.current;
  }

  // Underscore-prefixed to signal "used by rest.ts but not part of the public API."
  readonly _controlUrl: string;
  readonly _apiKey: string;
  readonly _fetch: typeof fetch;
  // Kept so a refresh can open the replacement connection the same way the
  // first one was opened — a Neon-backed client must stay Neon-backed.
  private readonly _driver: (dsn: string) => Sql | Promise<Sql>;

  private constructor(
    sql: Sql,
    schema: string,
    expiresAt: Date,
    controlUrl: string,
    apiKey: string,
    f: typeof fetch,
    driver: (dsn: string) => Sql | Promise<Sql>,
  ) {
    this.ref = { current: sql };
    this.schema = schema;
    this.expiresAt = expiresAt;
    this._controlUrl = controlUrl;
    this._apiKey = apiKey;
    this._fetch = f;
    this._driver = driver;
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
    const sql = await driver(body.dsn);
    const client = new PwrapClient(
      sql, body.schema, new Date(body.expires_at), controlUrl, cfg.apiKey, f, driver,
    );
    client.scheduleRefresh();
    return client;
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

  /**
   * Typed JSONB collection. `T` describes the document body, so `insert` and
   * `find` are checked against your own shape:
   *
   *   const notes = c.table<{ title: string; tags: string[] }>("notes");
   */
  table<T extends Record<string, unknown> = Record<string, unknown>>(collection: string): Table<T> {
    return new Table<T>(this.ref, collection);
  }

  vector(collection: string): Vector {
    return new Vector(this.ref, collection);
  }

  queue(): Queue {
    return new Queue(this.ref);
  }

  geo(collection: string): Geo {
    return new Geo(this.ref, collection);
  }

  async close(): Promise<void> {
    this.closed = true;
    if (this.refreshTimer !== undefined) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = undefined;
    }
    await this.ref.current.end({ timeout: 5 });
  }

  // --- credential refresh -----------------------------------------------

  /**
   * Re-exchange the API key before the current credentials expire.
   *
   * The timer is unref'd: a background refresh must never be the reason a
   * short-lived script fails to exit.
   */
  private scheduleRefresh(): void {
    if (this.closed) return;
    const delay = Math.max(
      this.expiresAt.getTime() - Date.now() - REFRESH_LEAD_MS,
      MIN_REFRESH_DELAY_MS,
    );
    this.refreshTimer = setTimeout(() => {
      void this.refresh();
    }, delay);
    this.refreshTimer.unref?.();
  }

  private async refresh(): Promise<void> {
    if (this.closed) return;
    try {
      const res = await this._fetch(`${this._controlUrl}/v1/connection`, {
        method: "POST",
        headers: { Authorization: `Bearer ${this._apiKey}` },
      });
      if (!res.ok) throw new Error(`pwrap: refresh failed: ${res.status} ${await res.text()}`);
      const body = (await res.json()) as ConnectionResp;

      const driver = this._driver;
      const next = await driver(body.dsn);
      const old = this.ref.current;
      // Swap first: handles read through the holder, so new work picks up the
      // new credentials immediately.
      this.ref.current = next;
      this.expiresAt = new Date(body.expires_at);

      setTimeout(() => {
        void old.end({ timeout: 5 }).catch(() => {});
      }, OLD_SQL_GRACE_MS).unref?.();
    } catch {
      // The current connection stays usable until its role actually expires,
      // so a failure is worth retrying rather than tearing the client down.
    } finally {
      this.scheduleRefresh();
    }
  }
}
