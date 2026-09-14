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

### The bootstrap token only mints admin tokens

`PWRAP_BOOTSTRAP_TOKEN` is the root credential, and its sole remaining power is
issuing, listing and revoking scoped admin tokens under `/v1/admin/tokens`. It
is rejected on every other management endpoint, so a leaked deployment secret no
longer hands over the whole management API.

Management calls carry a scoped token (`pwa_…`) instead, granting one or more of:

| Scope | Covers |
|---|---|
| `projects` | create, list, delete and branch projects |
| `keys` | issue, list and revoke project API keys |
| `migrate` | apply tenant migrations and read their status |
| `sql` | the arbitrary-SQL escape hatch |

`sql` is separate on purpose: it executes anything as the tenant role, so it is
the capability most worth withholding. Grant the narrowest set that does the job
— a CI pipeline that only runs migrations needs `migrate`, nothing more.

Tokens are stored the same way as project API keys: argon2id hash, an
8-character lookup prefix in the clear, plaintext shown once at issue. Revocation
takes effect on the next request.

Mint one with `pwrap admin token issue --scopes migrate`, then export it as
`PWRAP_ADMIN_TOKEN`.

Never expose the pwrapd management surface to the public internet. If the
bootstrap token is empty, `AdminOnly` refuses every request rather than
defaulting open.

### Admin actions are audited

Every mutating management request writes a row to `admin_audit_log`: the acting
token's prefix and name, the route, the project, and the resulting status.
Refusals are recorded too — a run of denials is exactly what an investigation
wants to see. Reads are skipped so the trail is not drowned in list calls.

The table stores the token *prefix*, never the token, so the log identifies the
actor without being worth stealing.

pwrapd prunes the log on its ten-minute housekeeping pass, dropping rows older
than `PWRAP_AUDIT_RETENTION_DAYS` (90 by default). Pick the window your
compliance obligations need rather than the one that keeps the table small — a
breach is usually discovered long after it happens, and a trail that has already
been pruned answers nothing. Setting the value to `0` disables pruning and keeps
rows forever, which is a legitimate choice as long as someone is watching the
table's size.

### `/v1/projects/{id}/sql` executes arbitrary SQL by design

It is the escape hatch for creating real tables that PostgREST can expose. The
body is executed verbatim as the tenant role inside a transaction. It is
admin-authenticated and must never be reachable with a project API key.

### DSN credentials are short-lived, but live sessions are not

`POST /v1/connection` mints a dedicated Postgres login role per exchange,
inheriting the tenant role and carrying `VALID UNTIL` set to the TTL (one hour
by default, `PWRAP_DSN_TTL_SECONDS`). Postgres refuses that credential once it
expires, so a leaked DSN stops working on its own, and one client's credential
can be dropped without touching anyone else's.

Two limits worth knowing:

- **Expiry bounds new connections, not open ones.** Postgres checks credentials
  at authentication, so a session already established keeps working until it
  closes. Shrinking the TTL shortens the window for reuse of a stolen DSN; it
  does not cut an attacker's existing connection.
- **Revoking an API key does not immediately kill DSNs it minted.** Those roles
  expire on their own schedule. To revoke now, drop the roles listed in
  `ephemeral_roles` for that project.

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
- **Bootstrap token comparison** is constant-time (`crypto/subtle`); scoped
  admin tokens are verified by argon2id, like project API keys.
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
