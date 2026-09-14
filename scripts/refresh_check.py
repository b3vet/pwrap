"""Prove the Python SDK re-exchanges credentials before the server expires them.

Driven by scripts/refresh-check.sh; nightly only, since proving a refresh means
outliving a credential.
"""
import asyncio
import os
import sys

from pwrap import PwrapClient


async def main() -> int:
    key = open("/tmp/refresh-key.txt").read().strip()
    wait = int(os.environ.get("PWRAP_REFRESH_WAIT", "105"))
    c = await PwrapClient.connect(api_key=key, control_url="http://localhost:8080")
    first = c.expires_at
    # Taken before any refresh on purpose: the pool swap has to reach handles
    # that already exist.
    notes = c.table("refreshcheck")
    await notes.insert({"n": 0})

    await asyncio.sleep(wait)

    try:
        await notes.insert({"n": 1})
    except Exception as e:  # noqa: BLE001
        print(f"FAIL: insert after {wait}s: {type(e).__name__}: {e}", file=sys.stderr)
        return 1
    if c.expires_at <= first:
        print(f"FAIL: expiry never advanced from {first.isoformat()}", file=sys.stderr)
        return 1
    print(f"  py: PASS (expiry {first.isoformat()} -> {c.expires_at.isoformat()})")
    await c.close()
    return 0


sys.exit(asyncio.run(main()))
