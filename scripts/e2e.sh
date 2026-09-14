#!/usr/bin/env bash
# End-to-end dogfood run. Assumes `docker-compose up -d postgres` is already running
# and that ./bin/{pwrapd,todo-plus} have been built (`make build`).
#
# Env:
#   PWRAP_DATABASE_URL      connection string for the pwrap admin role (default: local docker-compose)
#   PWRAP_BOOTSTRAP_TOKEN   admin token, required
set -euo pipefail

: "${PWRAP_DATABASE_URL:?}"
: "${PWRAP_BOOTSTRAP_TOKEN:?}"

# psql is invoked via `docker exec` against the docker-compose container so this
# script works on machines without a local libpq install (macOS out of the box).
PSQL() {
  docker exec -i pwrap-postgres psql -U pwrap -d pwrap -v ON_ERROR_STOP=1 "$@"
}

# Reset: drop any prior tenant role/schema, drop + recreate control-plane tables.
PSQL >/dev/null <<'SQL'
DO $$ DECLARE r RECORD; BEGIN
  FOR r IN SELECT nspname FROM pg_namespace WHERE nspname LIKE 'p\_%' ESCAPE '\' LOOP
    EXECUTE 'DROP SCHEMA IF EXISTS ' || quote_ident(r.nspname) || ' CASCADE';
  END LOOP;
END $$;
DO $$ DECLARE r RECORD; BEGIN
  FOR r IN SELECT rolname FROM pg_roles WHERE rolname LIKE 'p\_%' ESCAPE '\' LOOP
    EXECUTE 'DROP OWNED BY ' || quote_ident(r.rolname) || ' CASCADE';
    EXECUTE 'DROP ROLE IF EXISTS ' || quote_ident(r.rolname);
  END LOOP;
END $$;
DROP TABLE IF EXISTS migration_log CASCADE;
DROP TABLE IF EXISTS api_keys     CASCADE;
DROP TABLE IF EXISTS projects     CASCADE;
SQL

PSQL >/dev/null < migrations/controlplane/0001_init.up.sql

# Launch pwrapd, run the demo, shut it down.
./bin/pwrapd >/tmp/pwrapd.log 2>&1 </dev/null &
PWRAPD_PID=$!
# SIGTERM then SIGKILL: pwrapd can stall on shutdown if a realtime client died
# mid-subscribe, and a bare `wait` would hang the script indefinitely.
cleanup() {
  kill "$PWRAPD_PID" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$PWRAPD_PID" 2>/dev/null || return 0
    sleep 0.5
  done
  kill -9 "$PWRAPD_PID" 2>/dev/null || true
  wait "$PWRAPD_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for /readyz.
for _ in {1..40}; do
  if curl -sf http://localhost:8080/v1/readyz >/dev/null; then break; fi
  sleep 0.5
done
if ! curl -sf http://localhost:8080/v1/readyz >/dev/null; then
  echo "pwrapd never became ready; log follows:" >&2
  cat /tmp/pwrapd.log >&2
  exit 1
fi

# PostgREST authenticates as pwrap_authenticator, which pwrapd only creates at
# startup — so a sidecar started before pwrapd sits in a failed-password retry
# loop with a backoff that can outlast the demo. Now that the role exists, bounce
# it and wait for it to actually answer. Skipped when no sidecar is running,
# since most demos don't need one.
# `ps -a`, not `--status running`: when PostgREST is stuck in its failed-auth
# restart loop it is not "running", which is exactly the case this block exists
# to repair. Gating on running state skipped the fix precisely when it mattered.
if docker compose ps -a --services 2>/dev/null | grep -qx postgrest; then
  REST_URL="${PWRAP_REST_URL:-http://localhost:${PWRAP_PGRST_PORT:-3000}}"
  docker compose restart postgrest >/dev/null
  for _ in $(seq 1 60); do
    if curl -sf "$REST_URL/" >/dev/null 2>&1; then break; fi
    sleep 0.5
  done
  if ! curl -sf "$REST_URL/" >/dev/null 2>&1; then
    echo "postgrest never became ready at $REST_URL" >&2
    docker compose logs --tail 30 postgrest >&2
    exit 1
  fi
fi

# Which example binary to run. Default: todo-plus. Pass "rls-notes" for Track A demo.
"./bin/${1:-todo-plus}"
