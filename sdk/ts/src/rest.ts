import type { PwrapClient } from "./client.js";

export interface RestToken {
  token: string;
  url: string;
  role: string;
  expires_at: Date;
}

export interface RestTokenOpts {
  userId?: string;
  ttlSeconds?: number;
}

/**
 * Exchange the SDK's API key for a PostgREST-compatible JWT.
 *
 * The returned token is a signed HS256 JWT whose `role` claim tells PostgREST
 * which Postgres role to SET ROLE into. Attach it to requests as
 * `Authorization: Bearer <token>`.
 *
 * Throws with a helpful message if pwrapd was started without
 * PWRAP_JWT_SECRET / PWRAP_AUTHENTICATOR_PASSWORD.
 */
export async function issueRestToken(client: PwrapClient, opts: RestTokenOpts = {}): Promise<RestToken> {
  // PwrapClient doesn't currently expose its config; the caller supplies the control
  // URL + API key via a small helper on the client itself (see client.ts additions).
  const body = JSON.stringify({
    user_id: opts.userId ?? "",
    ttl_seconds: opts.ttlSeconds ?? 0,
  });
  const resp = await client._fetch(`${client._controlUrl}/v1/rest/token`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${client._apiKey}`,
    },
    body,
  });
  if (resp.status === 503) {
    throw new Error("pwrap: REST integration not enabled on this pwrapd");
  }
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`pwrap: /v1/rest/token ${resp.status} ${resp.statusText}: ${text}`);
  }
  const j = await resp.json() as { token: string; url: string; role: string; expires_at: string };
  return { token: j.token, url: j.url, role: j.role, expires_at: new Date(j.expires_at) };
}
