# Neon integration

pwrap runs on top of any Postgres 16+ cluster. When that cluster is **[Neon](https://neon.tech)**, the killer feature is *copy-on-write branches* — instant, isolated database forks. This document covers the pattern pwrap recommends for using them, and the wiring it provides.

## What pwrap ships

`pwrapd` itself doesn't talk to Neon's control plane. Instead the `pwrap` CLI exposes a Neon API wrapper (`pwrap neon …`) and pwrap's `PWRAP_DATABASE_URL` accepts a Neon connection string verbatim. Operators orchestrate Neon branches externally and point a fresh pwrapd at the resulting URL.

Wrapper coverage (subset of [Neon API v2](https://api-docs.neon.tech)):

| pwrap CLI | Neon endpoint |
|---|---|
| `pwrap neon branch create` | `POST /projects/{id}/branches` |
| `pwrap neon branch list`   | `GET  /projects/{id}/branches` |
| `pwrap neon branch delete` | `DELETE /projects/{id}/branches/{branch_id}` |
| `pwrap neon connection-uri`| `GET  /projects/{id}/connection_uri` |

The Go package `internal/neon` exposes the same surface programmatically.

Environment:

- `PWRAP_NEON_API_KEY` — Neon API token. Required for all `pwrap neon …` subcommands.
- `PWRAP_NEON_BASE_URL` — Override the default `https://console.neon.tech/api/v2`. Useful for tests and self-hosted Neon (the [open-source Neon control plane](https://github.com/neondatabase/neon)).

## Pattern 1 — Per-PR preview environment

CI script for "give every PR its own database":

```bash
#!/usr/bin/env bash
set -euo pipefail

export PWRAP_NEON_API_KEY="${NEON_API_KEY}"

BRANCH_NAME="ci-pr-${GITHUB_PR_NUMBER}"
NEON_PROJECT="${NEON_PROJECT_ID}"

# Create the Neon branch with a read-write endpoint.
pwrap neon branch create \
  --neon-project "$NEON_PROJECT" \
  --name "$BRANCH_NAME" > /tmp/branch.json
BRANCH_ID=$(jq -r .branch.id < /tmp/branch.json)

# Get the DSN. Use the pooled URI in CI so concurrent jobs don't exhaust connections.
DSN=$(pwrap neon connection-uri \
  --neon-project "$NEON_PROJECT" \
  --branch "$BRANCH_ID" \
  --role owner --database appdb --pooled)

# Spin up pwrapd against the branch and run tests against it.
export PWRAP_DATABASE_URL="$DSN"
export PWRAP_BOOTSTRAP_TOKEN="ci-admin-token"
export PWRAP_AUTHENTICATOR_PASSWORD="ci-authpw"
export PWRAP_JWT_SECRET="ci-jwt-32-bytes-please-please"

./bin/pwrapd &
PWRAPD_PID=$!
trap "kill $PWRAPD_PID; pwrap neon branch delete --neon-project $NEON_PROJECT --branch $BRANCH_ID" EXIT

# Apply control-plane schema (or use a real migration tool in front of it).
psql "$DSN" -f migrations/controlplane/0001_init.up.sql

# Now run your integration tests, your preview app, etc. against the isolated branch.
go test ./...
```

The branch and its endpoint are deleted in `trap` — Neon stops billing once the endpoint is gone (compute hours), and the storage shrinks to copy-on-write deltas.

## Pattern 2 — pwrap "branch" + Neon "branch" stacked

`pwrap branch` (see [examples/branching](../examples/branching/main.go)) creates a child *pwrap project* against the same cluster as the parent — useful for in-cluster forks where Neon-level branching isn't an option (e.g. local dev, single-region prod).

When you do want both — a Neon branch *and* a pwrap branch sitting on it — orchestrate in this order:

1. `pwrap neon branch create` → `BRANCH_DSN`
2. Spin up a second pwrapd pointing at `BRANCH_DSN` (or reconfigure existing pwrapd).
3. `pwrap project create --name your-project` against that pwrapd. This is your "branched" pwrap project; it lives entirely inside the Neon branch.

## Pattern 3 — Self-hosted Neon

Neon is Apache-licensed; you can run the control plane yourself. The pwrap wrapper accepts any base URL via `PWRAP_NEON_BASE_URL`, so the same CLI works against self-hosted Neon without code changes.

```bash
export PWRAP_NEON_API_KEY="..."
export PWRAP_NEON_BASE_URL="https://neon.internal.example.com/api/v2"
pwrap neon branch list --neon-project proj-prod
```

## What pwrap intentionally doesn't do

- **No Neon-aware project creation inside pwrapd.** pwrapd is single-cluster by design — schema-per-project, role-per-project. Multi-cluster routing is an operator concern. Adding it inside pwrapd would force a model where every API call also carries cluster identity; not worth it for a project at this stage.
- **No bundled Neon connection string in the SDK.** The SDK uses `/v1/connection` to fetch a scoped DSN from pwrapd, which already encapsulates "where Postgres is". If pwrapd points at Neon, the SDK does too — transparently.

## Testing the wrapper

The Neon client's HTTP behaviour is covered by table-driven tests against an `httptest.Server`:

```
go test ./internal/neon/... -v
```

For end-to-end CLI verification you can stand up the toy mock at `scripts/mock-neon.go` (carries a `//go:build ignore` tag, so it's excluded from normal builds) and point the CLI at it via `PWRAP_NEON_BASE_URL=http://localhost:9911`.
