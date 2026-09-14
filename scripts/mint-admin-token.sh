#!/usr/bin/env bash
# Mint a full-scope admin token and print it.
#
# The bootstrap token now only mints tokens, so every harness that drives the
# management API needs one of these first. Full scope is right for test
# harnesses; real deployments should grant narrowly.
set -euo pipefail
URL="${PWRAP_CONTROL_URL:-http://localhost:8080}"
BOOT="${PWRAP_BOOTSTRAP_TOKEN:-dev-admin}"
curl -sf -X POST "$URL/v1/admin/tokens" \
  -H "Authorization: Bearer $BOOT" \
  -H 'Content-Type: application/json' \
  -d '{"name":"harness","scopes":["projects","keys","migrate","sql"]}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])'
