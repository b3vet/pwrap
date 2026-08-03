# Contributing to pwrap

Thanks for taking a look. pwrap is pre-alpha and the public API will change, so
this is a good moment for contributions that shape it.

## Before you start

For anything beyond a bug fix or a typo, open an issue first. It's cheaper to
disagree about an approach in a paragraph than in a diff.

Good first contributions: filling gaps in test coverage (see below), rounding
out the SDKs so all three stay at parity, and correcting documentation that has
drifted from the code.

## Setup

You need Go 1.25+, Docker, and — if you're touching the SDKs — Node 20+ with
pnpm, and Python 3.11+.

```bash
git clone https://github.com/b3vet/pwrap.git
cd pwrap
cp .env.example .env      # PostgREST reads these from docker-compose
make pg                   # start Postgres (pgvector + pg_graphql + postgis)
make build
make test
```

`make help` lists every target.

## Repository layout

```
cmd/pwrapd            control-plane daemon
cmd/pwrap             CLI
internal/             control plane: projects, keys, migrations, branches,
                      realtime, REST tokens, crypto, telemetry
migrations/           embedded SQL — controlplane/ and tenant/ baselines
sdk/go  sdk/ts  sdk/py   client SDKs, kept at feature parity
examples/             runnable demos, doubling as end-to-end tests
```

The architecture is hybrid: SDKs talk to Postgres directly on the hot path,
while `pwrapd` owns projects, keys and migrations. If a change would put pwrapd
in the path of every query, it probably belongs somewhere else.

## Before you open a PR

```bash
make fmt
make lint             # golangci-lint v2
make test             # unit tests, race detector
make test-integration # testcontainers, incl. PostgREST; needs Docker
make conformance      # the same scenarios through all three SDKs
```

CI runs all of the above plus TypeScript typecheck/build. All six jobs must be
green. Slower checks — the end-to-end demos, a Postgres 16/17 matrix, and a
benchmark baseline — run nightly.

Some further expectations:

- **Keep the three SDKs at parity.** A new capability in the Go SDK should land
  in TypeScript and Python too, with the same method names and semantics. If you
  can only do one, record the gap in [`conformance/scenarios.json`](conformance/README.md)
  with a reason — the suite prints every tracked gap on every run, so a partial
  landing stays visible instead of quietly becoming permanent.
- **Update `openapi.yaml`** when you add, remove or change an HTTP endpoint. It
  is validated as OpenAPI 3.1 and it is meant to match the server exactly.
- **Migrations are append-only.** Add `NNNN_description.up.sql` and a matching
  `.down.sql`; never edit an applied migration.
- **Explain *why* in comments,** not *what*. The existing code does this — the
  reasoning behind a non-obvious decision is the part a reader can't reconstruct.

## Test coverage

Coverage is uneven, and honestly so: `crypto`, `keys`, `auth`, `authjwt`,
`tenancy`, `migrations`, `neon` and `telemetry` have unit tests, and the
testcontainers suite in `internal/integration` exercises the full stack. Several
packages — `projects`, `branches`, `realtime`, `sqlapply`, `authsetup` — have no
unit tests of their own and are covered only indirectly.

Tests for those are welcome and are the most useful contribution right now.

## Commits and PRs

Write commit subjects in the imperative mood ("Add branch sync endpoint"), and
use the body to explain why the change is needed. Keep a PR to one logical
change; if you find an unrelated problem along the way, open an issue for it.

## License

Contributions are licensed under Apache 2.0, matching the project.
