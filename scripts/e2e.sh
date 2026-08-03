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
./bin/pwrapd >/tmp/pwrapd.log 2>&1 &
PWRAPD_PID=$!
trap 'kill $PWRAPD_PID 2>/dev/null || true; wait $PWRAPD_PID 2>/dev/null || true' EXIT

# Wait for /readyz.
for _ in {1..20}; do
  if curl -sf http://localhost:8080/v1/readyz >/dev/null; then break; fi
  sleep 0.2
done

# Which example binary to run. Default: todo-plus. Pass "rls-notes" for Track A demo.
"./bin/${1:-todo-plus}"
