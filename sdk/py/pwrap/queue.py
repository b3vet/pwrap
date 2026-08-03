from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any, Optional

from .exceptions import PwrapError


@dataclass
class Stats:
    available: int = 0
    running: int = 0
    scheduled: int = 0
    completed: int = 0
    discarded: int = 0
    retryable: int = 0


class Queue:
    """Insert-only handle over the project's River-backed queue.

    Workers live in Go land. This SDK can enqueue jobs and read stats.
    """

    def __init__(self, client) -> None:
        self._client = client

    async def enqueue(
        self,
        *,
        kind: str,
        args: Optional[dict[str, Any]] = None,
        queue: str = "default",
        priority: int = 1,
        max_attempts: int = 25,
    ) -> int:
        if not kind:
            raise PwrapError("kind is required")
        args_json = json.dumps(args or {})

        async def fn(conn):
            return await conn.fetchval(
                """
                INSERT INTO river_job (kind, args, queue, priority, max_attempts)
                VALUES ($1, $2::jsonb, $3, $4, $5)
                RETURNING id
                """,
                kind, args_json, queue, priority, max_attempts,
            )

        row_id = await self._client._run(fn)
        return int(row_id)

    async def stats(self) -> Stats:
        async def fn(conn):
            return await conn.fetch(
                "SELECT state::text AS state, COUNT(*) AS n FROM river_job GROUP BY state"
            )

        rows = await self._client._run(fn)
        s = Stats()
        for r in rows:
            state = r["state"]
            if hasattr(s, state):
                setattr(s, state, int(r["n"]))
        return s


_ = field
