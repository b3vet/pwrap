import postgres from "postgres";
import { Table } from "./table.js";
import { Vector } from "./vector.js";
import { Queue } from "./queue.js";
import { Geo } from "./geo.js";
import { Matview } from "./matview.js";
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
 * How handles reach the database.
 *
 * Credentials are minted per exchange and expire server-side, so the client
 * replaces its connection before that happens. Handles read through this ref
 * rather than capturing an Sql, so one kept across a refresh keeps working.
 */
export interface SqlRef {
  /** The live connection. Replaced on refresh — read it, don't cache it. */
  readonly current: Sql;
  /**
   * Run one operation. Normally that is just `fn(current)`. On a ref bound to a
   * user (see {@link PwrapClient.withUser}) it instead runs inside a transaction
   * with `request.jwt.claims` set, so an RLS policy reading that claim behaves
   * the same whether the query arrived through the SDK or through PostgREST.
   */
  run<R>(fn: (sql: Sql) => Promise<R>): Promise<R>;
}

/** The single mutable cell a client and all its derived clients share. */
class ConnHolder {
  constructor(public current: Sql) {}
}

/**
 * A view onto the shared connection, optionally bound to a user id.
 *
 * Kept separate from the holder so `withUser` can hand out a differently-scoped
 * view of the *same* connection — a derived client must follow the parent's
 * credential refreshes, not pin the connection it was created with.
 */
class ScopedRef implements SqlRef {
  constructor(
    private readonly holder: ConnHolder,
    private readonly userId?: string,
  ) {}

  get current(): Sql {
    return this.holder.current;
  }

  async run<R>(fn: (sql: Sql) => Promise<R>): Promise<R> {
    const sql = this.holder.current;
    if (this.userId === undefined) return fn(sql);
    const claims = JSON.stringify({ user_id: this.userId });
    // The result is wrapped in an object on purpose: postgres.js resolves an
    // array returned from begin() through Promise.all, which would replace a
    // RowList with a plain array and drop the `count` that update/delete read.
    const out = await sql.begin(async (tx) => {
      await tx`SELECT set_config('request.jwt.claims', ${claims}, true)`;
      return { value: await fn(tx as unknown as Sql) };
    });
    return (out as unknown as { value: R }).value;
  }
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
  private readonly holder: ConnHolder;
  private readonly ref: SqlRef;
  private readonly _userId?: string;
  private refreshTimer?: ReturnType<typeof setTimeout>;
  private closed = false;

  /**
   * The live connection. Replaced on refresh — read it, don't cache it.
   *
   * This is the raw escape hatch: it is *not* scoped by `withUser`, because a
   * bare connection has nowhere to put the claim. Go through the handles
   * (`table`, `vector`, `geo`, `queue`) for user-scoped work.
   */
  get sql(): Sql {
    return this.holder.current;
  }

  /** The user id bound by {@link withUser}, or undefined on a root client. */
  get userId(): string | undefined {
    return this._userId;
  }

  // Underscore-prefixed to signal "used by rest.ts but not part of the public API."
  readonly _controlUrl: string;
  readonly _apiKey: string;
  readonly _fetch: typeof fetch;
  // Kept so a refresh can open the replacement connection the same way the
  // first one was opened — a Neon-backed client must stay Neon-backed.
  private readonly _driver: (dsn: string) => Sql | Promise<Sql>;

  private constructor(
    holder: ConnHolder,
    schema: string,
    expiresAt: Date,
    controlUrl: string,
    apiKey: string,
    f: typeof fetch,
    driver: (dsn: string) => Sql | Promise<Sql>,
    userId?: string,
  ) {
    this.holder = holder;
    this.ref = new ScopedRef(holder, userId);
    this._userId = userId;
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
      new ConnHolder(sql), body.schema, new Date(body.expires_at), controlUrl, cfg.apiKey, f, driver,
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
    // An explicit userId wins; otherwise a user-scoped client subscribes as that
    // user, so `c.withUser("alice").subscribe()` does the obvious thing.
    const sub = new Subscription(this, { ...opts, userId: opts.userId ?? this._userId });
    await sub.ready();
    return sub;
  }

  /**
   * Derive a client whose operations run as one user.
   *
   * Every query issued through the returned client's handles executes inside a
   * transaction with `request.jwt.claims` set to `{"user_id": userId}`, which is
   * the same claim PostgREST sets from a JWT — so one RLS policy covers both
   * paths. Without it, queries run unscoped and a FORCE RLS policy keyed on that
   * claim matches nothing.
   *
   *   const alice = c.withUser("alice");
   *   await alice.table("notes").insert({ user_id: "alice", body: "..." });
   *   await alice.table("notes").find();   // only alice's rows
   *
   * The derived client shares the parent's connection and its credential
   * refreshes. Don't close it — close the parent.
   */
  withUser(userId: string): PwrapClient {
    return new PwrapClient(
      this.holder,
      this.schema,
      this.expiresAt,
      this._controlUrl,
      this._apiKey,
      this._fetch,
      this._driver,
      userId,
    );
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

  /** Handle over a named materialized view in the pwrap_matviews registry. */
  matview(name: string): Matview {
    return new Matview(this.ref, name);
  }

  /**
   * Close the connection and stop refreshing it.
   *
   * A no-op on a client returned by {@link withUser} — those share the parent's
   * connection, and closing one would pull the connection out from under every
   * other scope. Close the client you called `connect` on.
   */
  async close(): Promise<void> {
    if (this._userId !== undefined) return;
    this.closed = true;
    if (this.refreshTimer !== undefined) {
      clearTimeout(this.refreshTimer);
      this.refreshTimer = undefined;
    }
    await this.holder.current.end({ timeout: 5 });
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
      const old = this.holder.current;
      // Swap first: handles read through the holder, so new work picks up the
      // new credentials immediately — derived clients from withUser included.
      this.holder.current = next;
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
