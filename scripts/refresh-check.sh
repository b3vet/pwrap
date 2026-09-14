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

# Pulls one field out of a control-plane response, and says what actually came
# back when the field is missing. Without this a 409 or a 401 surfaces as a bare
# `KeyError: id`, which names neither the request nor the reason.
api_field() {
  python3 -c '
import json, sys
field, what = sys.argv[1], sys.argv[2]
body = sys.stdin.read()
try:
    value = json.loads(body)[field]
except Exception:
    sys.exit("refresh-check: " + what + " failed: " + (body.strip() or "(empty response)"))
print(value)
' "$1" "$2"
}

# A unique name per run: project names are unique and are not deleted on the way
# out, so a fixed name made the second run 409 against the first.
PROJECT_NAME="refresh-check-$(date +%s)-$$"
PID=$(curl -s -X POST -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"$PROJECT_NAME\"}" "$PWRAP_CONTROL_URL/v1/projects" | api_field id "create project")
# Tear the project down on the way out, so repeated runs do not pile up schemas.
# Ordering matters: the delete needs pwrapd, which cleanup then stops.
trap 'curl -s -X DELETE -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" "$PWRAP_CONTROL_URL/v1/projects/$PID" >/dev/null 2>&1 || true; cleanup' EXIT
curl -sf -X POST -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" "$PWRAP_CONTROL_URL/v1/projects/$PID/migrations" >/dev/null
curl -s -X POST -H "Authorization: Bearer $PWRAP_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"refresh"}' "$PWRAP_CONTROL_URL/v1/projects/$PID/keys" \
  | api_field key "issue key" > /tmp/refresh-key.txt

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
