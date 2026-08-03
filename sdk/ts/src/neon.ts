// Subpath export `@pwrap/sdk/neon` — swap in Neon's serverless driver for edge runtimes
// (Cloudflare Workers, Vercel Edge) where TCP is unavailable.
//
// Usage:
//   import { PwrapClient } from "@pwrap/sdk";
//   import { neonDriverAsync } from "@pwrap/sdk/neon";
//   const c = await PwrapClient.connect({ apiKey, driver: neonDriverAsync });
//
// `@neondatabase/serverless` exposes a postgres.js-compatible template-tag API, so it
// plugs into PwrapClient.driver without adapters.

import type { Sql } from "./client.js";

type NeonFactory = (dsn: string, opts?: Record<string, unknown>) => Sql;

/**
 * Returns a driver factory that constructs a Neon-serverless postgres.js-shaped client.
 * Throws at call time if `@neondatabase/serverless` isn't installed.
 */
export async function neonDriverAsync(dsn: string): Promise<Sql> {
  // Dynamic import keeps the Neon dep truly optional.
  const mod = await import("@neondatabase/serverless").catch(() => {
    throw new Error(
      "pwrap/neon: @neondatabase/serverless is not installed. `npm i @neondatabase/serverless`",
    );
  });
  // @ts-expect-error — the serverless entry exports `neon` as a default-like export.
  const factory = (mod.neon ?? mod.default?.neon) as NeonFactory;
  return factory(dsn);
}
