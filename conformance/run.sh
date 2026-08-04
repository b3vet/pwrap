#!/usr/bin/env bash
# Run the cross-SDK conformance suite against a live stack.
#
# Brings up Postgres + PostgREST via docker compose, boots pwrapd, runs the Go,
# TypeScript and Python runners, then gates on conformance/check.py.
#
# Used unchanged by CI and by developers, so a green local run means a green CI
# run. Assumes `make build` has produced ./bin/pwrapd.
#
#   ./conformance/run.sh          # everything
#   ./conformance/run.sh go py    # only the named runners (check.py still runs)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

REPORT_DIR="${PWRAP_REPORT_DIR:-$ROOT/conformance/.reports}"
export PWRAP_REPORT_DIR
PWRAP_REPORT_DIR="$REPORT_DIR"

# Non-default host ports by default: 5432 and 3000 are very often occupied by
# another project, and a port clash here looks like a pwrap failure.
export PWRAP_PG_PORT="${PWRAP_PG_PORT:-55432}"
export PWRAP_PGRST_PORT="${PWRAP_PGRST_PORT:-53000}"

export PWRAP_DATABASE_URL="${PWRAP_DATABASE_URL:-postgres://pwrap:pwrap@localhost:${PWRAP_PG_PORT}/pwrap?sslmode=disable}"
export PWRAP_REST_URL="${PWRAP_REST_URL:-http://localhost:${PWRAP_PGRST_PORT}}"
export PWRAP_BOOTSTRAP_TOKEN="${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}"
export PWRAP_AUTHENTICATOR_PASSWORD="${PWRAP_AUTHENTICATOR_PASSWORD:-authpw-dev}"
export PWRAP_JWT_SECRET="${PWRAP_JWT_SECRET:-super-dev-jwt-secret-please-change-me-32bytes}"
export PWRAP_ENCRYPTION_KEY="${PWRAP_ENCRYPTION_KEY:-MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=}"
export PWRAP_CONTROL_URL="${PWRAP_CONTROL_URL:-http://localhost:8080}"

RUNNERS=("$@")
if [ ${#RUNNERS[@]} -eq 0 ]; then
  RUNNERS=(go ts py)
fi

# Every long-running command gets a wall clock. A hang that produces no output
# is the worst failure mode here — it looks identical to slow progress, and on a
# CI runner it burns the whole job budget before anyone sees a log line.
RUNNER_TIMEOUT="${PWRAP_RUNNER_TIMEOUT:-300}"
INSTALL_TIMEOUT="${PWRAP_INSTALL_TIMEOUT:-300}"

# `timeout` is GNU coreutils; macOS has it as gtimeout, or not at all.
if command -v timeout >/dev/null 2>&1; then
  TIMEOUT=timeout
elif command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT=gtimeout
else
  TIMEOUT=""
fi
# Redirect stdin from /dev/null too: a package manager that decides to prompt
# should fail, not wait forever for an answer that will never come.
run_bounded() { # run_bounded <seconds> <cmd...>
  local secs="$1"; shift
  if [ -n "$TIMEOUT" ]; then
    "$TIMEOUT" --foreground "$secs" "$@" </dev/null
  else
    "$@" </dev/null
  fi
}

rm -rf "$REPORT_DIR"
mkdir -p "$REPORT_DIR" "$REPORT_DIR/ws-fallback"

log() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

# --- stack -------------------------------------------------------------------

log "starting postgres + postgrest"
# PostgREST reads the authenticator password from .env via docker compose.
if [ ! -f .env ]; then
  cp .env.example .env
fi
run_bounded "$INSTALL_TIMEOUT" docker compose up -d postgres postgrest

log "waiting for postgres"
pg_ready=0
for _ in $(seq 1 60); do
  if docker compose exec -T postgres pg_isready -U pwrap -d pwrap >/dev/null 2>&1; then pg_ready=1; break; fi
  sleep 1
done
if [ "$pg_ready" -ne 1 ]; then
  echo "postgres never became ready" >&2
  docker compose logs --tail 50 postgres >&2
  exit 1
fi

log "applying control-plane schema"
run_bounded 120 docker compose exec -T postgres \
  psql -U pwrap -d pwrap -v ON_ERROR_STOP=1 -q -f - \
  < migrations/controlplane/0001_init.up.sql

log "starting pwrapd"
# stdin from /dev/null and both streams to a file, so the daemon holds none of
# this script's descriptors — otherwise a caller reading our output blocks until
# pwrapd exits, long after the script itself is done.
./bin/pwrapd >"$REPORT_DIR/pwrapd.log" 2>&1 </dev/null &
PWRAPD_PID=$!
# Escalate to SIGKILL rather than waiting indefinitely on a graceful shutdown.
# pwrapd's realtime hub holds a LISTEN connection and can stall on SIGTERM if a
# WebSocket client died mid-subscribe; a plain `kill` + `wait` then blocks
# forever, and a CI runner reports it as an orphan process long after the suite
# has finished printing its results.
cleanup() {
  [ -n "${PWRAPD_PID:-}" ] || return 0
  kill "$PWRAPD_PID" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$PWRAPD_PID" 2>/dev/null || return 0
    sleep 0.5
  done
  echo "pwrapd did not exit on SIGTERM after 10s; killing" >&2
  kill -9 "$PWRAPD_PID" 2>/dev/null || true
  wait "$PWRAPD_PID" 2>/dev/null || true
}
trap cleanup EXIT

for _ in $(seq 1 60); do
  if curl -sf "$PWRAP_CONTROL_URL/v1/readyz" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
if ! curl -sf "$PWRAP_CONTROL_URL/v1/readyz" >/dev/null 2>&1; then
  echo "pwrapd never became ready; log follows:" >&2
  cat "$REPORT_DIR/pwrapd.log" >&2
  exit 1
fi

# PostgREST may have raced pwrapd creating the authenticator role. Once pwrapd is
# up the role exists, so a restart is guaranteed to land.
docker compose restart postgrest >/dev/null
for _ in $(seq 1 60); do
  if curl -sf "$PWRAP_REST_URL/" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
if ! curl -sf "$PWRAP_REST_URL/" >/dev/null 2>&1; then
  echo "postgrest never became ready at $PWRAP_REST_URL" >&2
  docker compose logs --tail 30 postgrest >&2
  exit 1
fi

# --- runners -----------------------------------------------------------------

# A runner failure must not stop the others: a partial report is still useful,
# and check.py reports every gap at once rather than one per run.
status=0

for r in "${RUNNERS[@]}"; do
  case "$r" in
    go)
      log "conformance: go"
      run_bounded "$RUNNER_TIMEOUT" go run ./conformance/go || status=1
      ;;
    ts)
      log "conformance: typescript"
      # --frozen-lockfile so a cold CI install can't silently resolve something
      # different from what was tested locally.
      ( cd sdk/ts        && run_bounded "$INSTALL_TIMEOUT" pnpm install --frozen-lockfile --reporter=silent ) || { status=1; continue; }
      ( cd sdk/ts        && run_bounded "$INSTALL_TIMEOUT" pnpm run build >/dev/null )                        || { status=1; continue; }
      ( cd conformance/ts && run_bounded "$INSTALL_TIMEOUT" pnpm install --reporter=silent )                  || { status=1; continue; }
      ( cd conformance/ts && run_bounded "$RUNNER_TIMEOUT"  pnpm start )                                      || status=1

      # Re-run with the global WebSocket removed, which is what Node 18 and 20
      # see. On a modern Node the first pass only ever exercises the global;
      # without this the `ws` fallback would rot unnoticed until a user on an
      # older runtime hit it. Report goes to a scratch dir so the parity gate
      # still reads exactly one report per SDK.
      log "conformance: typescript (ws fallback, no global WebSocket)"
      ( cd conformance/ts && PWRAP_REPORT_DIR="$REPORT_DIR/ws-fallback" \
          run_bounded "$RUNNER_TIMEOUT" pnpm start:no-global-ws ) || status=1
      ;;
    py)
      log "conformance: python"
      # CI pip-installs the SDK, so python3 already works. Locally it usually
      # doesn't, and a bare ModuleNotFoundError looks like a pwrap failure — so
      # fall back to a throwaway venv rather than making the developer guess.
      PY="${PWRAP_PYTHON:-python3}"
      if ! "$PY" -c "import pwrap" >/dev/null 2>&1; then
        venv="$ROOT/conformance/.venv"
        if [ ! -x "$venv/bin/python" ]; then
          log "installing the python sdk into $venv"
          python3 -m venv "$venv"
        fi
        run_bounded "$INSTALL_TIMEOUT" "$venv/bin/pip" install -q -e sdk/py || { status=1; continue; }
        PY="$venv/bin/python"
      fi
      run_bounded "$RUNNER_TIMEOUT" "$PY" conformance/py/runner.py || status=1
      ;;
    *)
      echo "unknown runner: $r" >&2
      exit 2
      ;;
  esac
done

# --- gate --------------------------------------------------------------------

log "parity gate"
python3 conformance/check.py "$REPORT_DIR" || status=1

exit $status
