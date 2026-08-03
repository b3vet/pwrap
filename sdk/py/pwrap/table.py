from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Optional

from .exceptions import NotFoundError


@dataclass
class Document:
    id: str
    data: dict[str, Any]
    created_at: datetime
    updated_at: datetime


class Table:
    """JSONB document collection backed by pwrap_documents + GIN."""

    def __init__(self, client, collection: str) -> None:
        self._client = client
        self._collection = collection

    async def insert(self, data: dict[str, Any]) -> str:
        payload = json.dumps(data)

        async def fn(conn):
            return await conn.fetchval(
                """
                INSERT INTO pwrap_documents (collection, data)
                VALUES ($1, $2::jsonb)
                RETURNING id
                """,
                self._collection, payload,
            )

        row_id = await self._client._run(fn)
        return str(row_id)

    async def insert_many(self, datas: list[dict[str, Any]]) -> list[str]:
        """Bulk-insert; returns generated ids in input order.

        Single round-trip per call via Postgres `unnest()` — scales beyond the
        per-query parameter limit.
        """
        if not datas:
            return []
        payloads = [json.dumps(d) for d in datas]

        async def fn(conn):
            return await conn.fetch(
                """
                INSERT INTO pwrap_documents (collection, data)
                SELECT $1, d::jsonb
                  FROM unnest($2::text[]) WITH ORDINALITY AS u(d, ord)
                 ORDER BY ord
                RETURNING id
                """,
                self._collection, payloads,
            )

        rows = await self._client._run(fn)
        return [str(r["id"]) for r in rows]

    async def get(self, doc_id: str) -> Optional[Document]:
        async def fn(conn):
            return await conn.fetchrow(
                """
                SELECT id, data, created_at, updated_at
                FROM pwrap_documents
                WHERE collection = $1 AND id = $2
                """,
                self._collection, doc_id,
            )

        row = await self._client._run(fn)
        if row is None:
            return None
        return _row_to_document(row)

    async def find(self, filter: Optional[dict[str, Any]] = None, limit: int = 100) -> list[Document]:
        if filter is None:
            filter = {}
        filt = json.dumps(filter)

        async def fn(conn):
            return await conn.fetch(
                """
                SELECT id, data, created_at, updated_at
                FROM pwrap_documents
                WHERE collection = $1 AND data @> $2::jsonb
                ORDER BY created_at DESC
                LIMIT $3
                """,
                self._collection, filt, limit,
            )

        rows = await self._client._run(fn)
        return [_row_to_document(r) for r in rows]

    async def update(self, doc_id: str, patch: dict[str, Any]) -> None:
        patch_json = json.dumps(patch)

        async def fn(conn):
            tag = await conn.execute(
                """
                UPDATE pwrap_documents
                   SET data = data || $3::jsonb, updated_at = now()
                 WHERE collection = $1 AND id = $2
                """,
                self._collection, doc_id, patch_json,
            )
            # asyncpg returns "UPDATE 0" or "UPDATE 1" — read the trailing int.
            return int(tag.rsplit(" ", 1)[-1])

        affected = await self._client._run(fn)
        if affected == 0:
            raise NotFoundError(f"document {doc_id!r} not found in {self._collection!r}")

    async def delete(self, doc_id: str) -> None:
        async def fn(conn):
            tag = await conn.execute(
                "DELETE FROM pwrap_documents WHERE collection = $1 AND id = $2",
                self._collection, doc_id,
            )
            return int(tag.rsplit(" ", 1)[-1])

        affected = await self._client._run(fn)
        if affected == 0:
            raise NotFoundError(f"document {doc_id!r} not found in {self._collection!r}")

    async def count(self, filter: Optional[dict[str, Any]] = None) -> int:
        if filter is None:
            filter = {}
        filt = json.dumps(filter)

        async def fn(conn):
            return await conn.fetchval(
                """
                SELECT COUNT(*) FROM pwrap_documents
                WHERE collection = $1 AND data @> $2::jsonb
                """,
                self._collection, filt,
            )

        n = await self._client._run(fn)
        return int(n)


def _row_to_document(row) -> Document:
    raw = row["data"]
    if isinstance(raw, str):
        raw = json.loads(raw)
    return Document(
        id=str(row["id"]),
        data=raw,
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )


# Suppress unused import warning by re-exporting `field`.
_ = field
