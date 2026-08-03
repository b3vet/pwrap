# Cross-SDK conformance suite

pwrap ships three SDKs and claims they behave the same. This suite is what makes
that claim checkable instead of aspirational.

```bash
make conformance          # all three SDKs
make conformance-go       # one runner; the gate still runs
```

Needs Docker, Go, Node with pnpm, and Python 3.11+. The suite brings up Postgres
and PostgREST on non-default ports (55432 and 53000, overridable via
`PWRAP_PG_PORT` / `PWRAP_PGRST_PORT`) so it won't collide with a Postgres you
already have running.

## How it works

`scenarios.json` is the canonical registry: one entry per behaviour, listing the
SDKs that implement it **today**. Each runner executes its scenarios against a
live stack and writes `report-<sdk>.json`. `check.py` then compares the reports
against the registry.

```
scenarios.json ──┬──> conformance/go   ──> report-go.json ──┐
                 ├──> conformance/ts   ──> report-ts.json ──┼──> check.py
                 └──> conformance/py   ──> report-py.json ──┘
```

The gate fails when:

- an SDK declares a scenario but doesn't report it, or reports a failure;
- an SDK **passes** a scenario the registry lists as missing from it — meaning
  the gap was closed and `scenarios.json` was never updated;
- a runner reports a scenario the registry has never heard of.

That last pair matters as much as the first. A registry that silently drifts out
of date is worse than no registry.

## Parity gaps are data, not silence

Scenarios an SDK doesn't implement go in `missing_from` with a reason:

```json
{
  "id": "realtime_subscribe",
  "sdks": ["go", "ts"],
  "missing_from": {
    "py": "Python SDK has no subscribe; would need a websocket dependency"
  }
}
```

`check.py` prints every gap on every run, so they stay visible. Closing one means
moving the SDK from `missing_from` into `sdks` — after which the gate enforces it
forever.

## Adding a scenario

1. Add it to `scenarios.json`, listing only the SDKs that will implement it now.
2. Implement it in each of those runners, under the same `id`.
3. Run `make conformance`.

Keep the runners behaviourally identical. The vector scenarios, for instance,
use the same `sin(seed + i*0.001)` embedding in all three languages, so the SDKs
are compared on identical inputs rather than merely similar ones — a difference
in results is then a real difference in behaviour.
