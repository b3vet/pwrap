import type { PwrapClient } from "./client.js";

export interface ChangeEvent {
  log_id: number;
  schema: string;
  table: string;
  op: "INSERT" | "UPDATE" | "DELETE";
  row_id?: string;
  user_id?: string;
  before?: Record<string, unknown> | null;
  after?: Record<string, unknown> | null;
  created_at: string;
}

export interface SubscribeOpts {
  table?: string;
  userId?: string;
}

/**
 * Subscription is an AsyncIterable of ChangeEvents. Iterate it until the loop
 * terminates (close()/server hangup), or call close() to stop early.
 *
 *     const sub = await client.subscribe({ table: "notes" });
 *     for await (const ev of sub) console.log(ev.op, ev.row_id);
 *
 * The Subscription auto-reconnects on transient WebSocket failures with
 * exponential backoff. Permanent errors (e.g. invalid API key) end the iteration.
 */
export class Subscription implements AsyncIterable<ChangeEvent> {
  private queue: ChangeEvent[] = [];
  private waiters: ((ev: IteratorResult<ChangeEvent>) => void)[] = [];
  private closed = false;
  private ws?: WebSocket;
  private abort = new AbortController();
  private helloResolve!: () => void;
  private helloReject!: (e: unknown) => void;
  private helloPromise: Promise<void>;
  private opts: SubscribeOpts;
  private url: string;

  constructor(private client: PwrapClient, opts: SubscribeOpts) {
    this.opts = opts;
    this.helloPromise = new Promise((res, rej) => {
      this.helloResolve = res;
      this.helloReject = rej;
    });
    this.url = this.buildURL();
    void this.runLoop();
  }

  /** Resolves once the first WebSocket handshake + hello frame complete. */
  ready(): Promise<void> { return this.helloPromise; }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.abort.abort();
    try { this.ws?.close(); } catch { /* ignore */ }
    // Wake any pending iterator with a done signal.
    while (this.waiters.length > 0) {
      const w = this.waiters.shift()!;
      w({ value: undefined as unknown as ChangeEvent, done: true });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<ChangeEvent> {
    return {
      next: (): Promise<IteratorResult<ChangeEvent>> => {
        if (this.queue.length > 0) {
          return Promise.resolve({ value: this.queue.shift()!, done: false });
        }
        if (this.closed) {
          return Promise.resolve({ value: undefined as unknown as ChangeEvent, done: true });
        }
        return new Promise((resolve) => this.waiters.push(resolve));
      },
      return: async () => {
        this.close();
        return { value: undefined as unknown as ChangeEvent, done: true };
      },
    };
  }

  // --- internals --------------------------------------------------------

  private deliver(ev: ChangeEvent): void {
    if (this.waiters.length > 0) {
      const w = this.waiters.shift()!;
      w({ value: ev, done: false });
      return;
    }
    this.queue.push(ev);
  }

  private buildURL(): string {
    const base = this.client._controlUrl.replace(/^http(s?):\/\//, (_m, s: string) => `ws${s}://`);
    const u = new URL(base + "/v1/subscribe");
    u.searchParams.set("api_key", this.client._apiKey);
    if (this.opts.table) u.searchParams.set("table", this.opts.table);
    if (this.opts.userId) u.searchParams.set("user_id", this.opts.userId);
    return u.toString();
  }

  private async runLoop(): Promise<void> {
    let backoff = 1000;
    let receivedHello = false;
    while (!this.closed) {
      try {
        const closeReason = await this.runOnce(() => { receivedHello = true; this.helloResolve(); });
        if (closeReason === "permanent") break;
        // transient close — fall through to reconnect
      } catch (err) {
        if (this.closed) break;
        // First connection attempt failed and we never sent hello — surface to caller.
        if (!receivedHello) {
          this.helloReject(err);
          this.close();
          return;
        }
      }
      if (this.closed) break;
      await sleep(backoff, this.abort.signal);
      backoff = Math.min(backoff * 2, 30000);
    }
    this.close();
  }

  private runOnce(onHello: () => void): Promise<"graceful" | "transient" | "permanent"> {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(this.url);
      this.ws = ws;
      let helloSeen = false;

      ws.addEventListener("open", () => { /* nothing — wait for hello */ });

      ws.addEventListener("message", (m: MessageEvent) => {
        try {
          const data = typeof m.data === "string" ? m.data : new TextDecoder().decode(m.data as ArrayBuffer);
          const parsed = JSON.parse(data);
          if (!helloSeen) {
            // First frame is the hello envelope ({type:"hello", ...}).
            helloSeen = true;
            onHello();
            return;
          }
          this.deliver(parsed as ChangeEvent);
        } catch (err) {
          // Bad frame; ignore.
        }
      });

      // CloseEvent is browser-only; in Node WebSocket fires close with {code,reason}
      // shaped similarly. Type as `unknown` and pull the code defensively.
      ws.addEventListener("close", (e: unknown) => {
        const code = (e as { code?: number } | undefined)?.code;
        if (code === 1008) {
          resolve("permanent");
          return;
        }
        resolve("transient");
      });

      ws.addEventListener("error", () => {
        // Errors surface as a close event too; let close() handle it.
      });

      this.abort.signal.addEventListener("abort", () => {
        try { ws.close(); } catch { /* ignore */ }
      }, { once: true });
    });
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => { clearTimeout(t); resolve(); }, { once: true });
  });
}
