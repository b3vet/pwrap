# Security

pwrap is **pre-alpha**. It provisions Postgres roles and schemas and executes
user-supplied SQL, so the trust model below is worth reading before you point it
at anything you care about.

## Reporting a vulnerability

Please report security issues privately via
[GitHub's private vulnerability reporting](https://github.com/b3vet/pwrap/security/advisories/new)
rather than a public issue.

Include a description, affected version or commit, and steps to reproduce.
Expect an initial response within 7 days. As a pre-1.0 single-maintainer
project there is no formal SLA and no backport policy: fixes land on `main` and
go out in the next release.

## Supported versions

Only the latest release is supported. There are no security backports to
earlier tags.

## Trust model

Understanding these boundaries matters more than any individual bug.

### The bootstrap token is a root credential

`PWRAP_BOOTSTRAP_TOKEN` is a single shared secret guarding every management
endpoint: creating and deleting projects, issuing and revoking API keys, running
migrations, branching, and `POST /v1/projects/{id}/sql`. There are no scopes, no
per-operator identities, and no audit trail of who used it.

Never expose the pwrapd management surface to the public internet. If the token
is empty, `AdminOnly` refuses every request rather than defaulting open.

### `/v1/projects/{id}/sql` executes arbitrary SQL by design

It is the escape hatch for creating real tables that PostgREST can expose. The
body is executed verbatim as the tenant role inside a transaction. It is
admin-authenticated and must never be reachable with a project API key.

### An issued DSN outlives the API key that minted it

`POST /v1/connection` returns a DSN containing the tenant role's **stable**
password. The `expires_at` field is a refresh hint for the SDK — nothing
enforces it server-side. Consequences:

- The DSN keeps working after `expires_at` passes.
- **Revoking the API key does not revoke a DSN already handed out.**

If a DSN leaks, rotate the tenant role's password in Postgres directly.
Rotating per-issue credentials are on the roadmap.

### Tenant isolation is Postgres-native

Each project gets its own role and schema (`p_<slug>`), and the role's
`search_path` scopes it. Isolation is exactly as strong as Postgres role
permissions — pwrap adds no application-layer filtering on the hot path, because
the SDK talks to Postgres directly. RLS policies you define are enforced by
Postgres for both the SDK and PostgREST.

### Realtime auth travels in the query string

`GET /v1/subscribe` accepts `?api_key=…` because browsers cannot set headers on
a `WebSocket` constructor. Query strings are routinely captured in proxy and
server access logs. Prefer the `Authorization` header wherever you control the
client.

## What is hardened

- **API keys** are `pwk_` + 32 random bytes (base64url), stored as argon2id
  hashes. Only an 8-character lookup prefix is stored in the clear. Plaintext is
  returned exactly once, at issue time.
- **Admin token comparison** is constant-time (`crypto/subtle`).
- **Credentials at rest**: `projects.pg_password` is encrypted with AES-256-GCM
  under an operator-supplied `PWRAP_ENCRYPTION_KEY`, in a versioned envelope
  (`v1:<base64>`) so the algorithm can be rotated. Legacy plaintext rows are
  re-encrypted at startup once a key is configured.
- **Rate limiting**: `/v1/connection`, `/v1/rest/token` and `/v1/subscribe` are
  limited to 10 rps (burst 20) per API-key prefix. The limiter runs *before*
  authentication so a flood of invalid keys cannot force argon2id verification
  on every request.
- **JWTs** for PostgREST are HS256, short-lived (default 1h), and carry only
  `role`, `project_id`, optional `user_id`, `iat` and `exp`.

## Production checklist

- [ ] Set `PWRAP_ENCRYPTION_KEY` (32 bytes, base64). Without it, tenant
      passwords are stored in cleartext and pwrapd warns at startup.
- [ ] Set a strong, unique `PWRAP_BOOTSTRAP_TOKEN`. Never reuse the `.env.example` value.
- [ ] Set a strong `PWRAP_JWT_SECRET`. It must match `PGRST_JWT_SECRET` on the
      PostgREST side.
- [ ] Keep the pwrapd management port off the public internet.
- [ ] Set `PWRAP_TENANT_SSLMODE=require` (or stricter). It defaults to `disable`
      for local development.
- [ ] Lower `PWRAP_TRACE_SAMPLE_RATE` from its default of `1.0`.
- [ ] Rotate any credential that has ever appeared in a `.env.example`,
      docker-compose default, or CI log.
