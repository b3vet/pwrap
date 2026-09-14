#!/usr/bin/env bash
# Verify that each SDK re-exchanges its credentials before they expire.
#
# This cannot live in the per-PR conformance suite: proving a refresh happened
# means outliving a credential, and the shortest honest version of that still
# costs about two minutes per SDK. It runs nightly instead.
#
# The server is started with a TTL shorter than the SDKs' refresh lead, which
# forces their minimum-delay floor and makes a refresh happen within ~30s. Each
# client then does work through a handle obtained BEFORE the refresh, after the
# original credential's lifetime has passed — so a pass means the swap reached
# existing handles, not just newly created ones.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TTL="${PWRAP_DSN_TTL_SECONDS:-90}"
WAIT="${PWRAP_REFRESH_WAIT:-105}"
export PWRAP_PG_PORT="${PWRAP_PG_PORT:-55432}"
export PWRAP_DSN_TTL_SECONDS="$TTL"
export PWRAP_DATABASE_URL="postgres://pwrap:pwrap@localhost:${PWRAP_PG_PORT}/pwrap?sslmode=disable"
export PWRAP_BOOTSTRAP_TOKEN=dev-admin
export PWRAP_CONTROL_URL=http://localhost:8080

docker compose up -d postgres >/dev/null
until docker compose exec -T postgres pg_isready -U pwrap -d pwrap >/dev/null 2>&1; do sleep 2; done
docker compose exec -T postgres psql -U pwrap -d pwrap -q -f - < migrations/controlplane/0001_init.up.sql >/dev/null

./bin/pwrapd >/tmp/refresh-pwrapd.log 2>&1 </dev/null &
PWRAPD_PID=$!
cleanup() {
  kill "$PWRAPD_PID" 2>/dev/null || true
  for _ in $(seq 1 20); do kill -0 "$PWRAPD_PID" 2>/dev/null || break; sleep 0.5; done
  kill -9 "$PWRAPD_PID" 2>/dev/null || true
}
trap cleanup EXIT
until curl -sf "$PWRAP_CONTROL_URL/v1/readyz" >/dev/null 2>&1; do sleep 1; done

# The bootstrap token only mints admin tokens now; everything else needs a
# scoped one. Full scope here because the harness drives every surface.
PWRAP_ADMIN_TOKEN="$(bash scripts/mint-admin-token.sh)"
export PWRAP_ADMIN_TOKEN

PID=$(curl -s -X POST -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"refresh-check"}' "$PWRAP_CONTROL_URL/v1/projects" | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -sf -X POST -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" "$PWRAP_CONTROL_URL/v1/projects/$PID/migrations" >/dev/null
curl -s -X POST -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"refresh"}' "$PWRAP_CONTROL_URL/v1/projects/$PID/keys" \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["key"])' > /tmp/refresh-key.txt

export PWRAP_REFRESH_WAIT="$WAIT"
status=0
for sdk in "$@"; do
  echo ""
  echo "==> refresh check: $sdk"
  case "$sdk" in
    go) go run ./scripts/refreshcheck || status=1 ;;
    py) "${PWRAP_PYTHON:-python3}" scripts/refresh_check.py || status=1 ;;
    ts) ( cd conformance/ts && npx tsx refresh-check.ts ) || status=1 ;;
    *)  echo "unknown sdk: $sdk" >&2; exit 2 ;;
  esac
done
exit $status
