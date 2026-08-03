from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, Optional

from .exceptions import NotFoundError, PwrapError

VECTOR_DIM = 1536


@dataclass
class Match:
    doc_id: str
    metadata: dict[str, Any]
    distance: float


class Vector:
    """pgvector-backed collection with HNSW search."""

    def __init__(self, client, collection: str) -> None:
        self._client = client
        self._collection = collection

    async def upsert_many(
        self,
        records: list[dict[str, Any]],
    ) -> None:
        """Bulk upsert. Each record is ``{"doc_id": str, "embedding": list[float], "metadata"?: dict}``.

        All embeddings must be VECTOR_DIM long — mismatches raise before touching the DB.
        Single round-trip per call via Postgres `unnest()`.
        """
        if not records:
            return
        doc_ids: list[str] = []
        embs: list[str] = []
        metas: list[str] = []
        for i, r in enumerate(records):
            emb = r["embedding"]
            if len(emb) != VECTOR_DIM:
                raise PwrapError(
                    f"record {i}: expected dim {VECTOR_DIM}, got {len(emb)}"
                )
            doc_ids.append(r["doc_id"])
            embs.append("[" + ",".join(repr(float(x)) for x in emb) + "]")
            metas.append(json.dumps(r.get("metadata") or {}))

        async def fn(conn):
            await conn.execute(
                """
                INSERT INTO pwrap_embeddings (collection, doc_id, embedding, metadata)
                SELECT $1, doc_id, emb::vector, meta::jsonb
                  FROM unnest($2::text[], $3::text[], $4::text[]) AS t(doc_id, emb, meta)
                    ON CONFLICT (collection, doc_id) DO UPDATE
                       SET embedding  = EXCLUDED.embedding,
                           metadata   = EXCLUDED.metadata,
                           created_at = now()
                """,
                self._collection, doc_ids, embs, metas,
            )

        await self._client._run(fn)

    async def upsert(
        self,
        doc_id: str,
        embedding: list[float],
        metadata: Optional[dict[str, Any]] = None,
    ) -> None:
        if len(embedding) != VECTOR_DIM:
            raise PwrapError(f"expected dim {VECTOR_DIM}, got {len(embedding)}")
        meta = json.dumps(metadata or {})
        lit = "[" + ",".join(repr(float(x)) for x in embedding) + "]"

        async def fn(conn):
            await conn.execute(
                """
                INSERT INTO pwrap_embeddings (collection, doc_id, embedding, metadata)
                VALUES ($1, $2, $3::vector, $4::jsonb)
                ON CONFLICT (collection, doc_id) DO UPDATE
                   SET embedding = EXCLUDED.embedding,
                       metadata  = EXCLUDED.metadata,
                       created_at = now()
                """,
                self._collection, doc_id, lit, meta,
            )

        await self._client._run(fn)

    async def delete(self, doc_id: str) -> None:
        async def fn(conn):
            tag = await conn.execute(
                "DELETE FROM pwrap_embeddings WHERE collection = $1 AND doc_id = $2",
                self._collection, doc_id,
            )
            return int(tag.rsplit(" ", 1)[-1])

        affected = await self._client._run(fn)
        if affected == 0:
            raise NotFoundError(f"vector {doc_id!r} not found in {self._collection!r}")

    async def search(self, query: list[float], k: int = 10) -> list[Match]:
        if len(query) != VECTOR_DIM:
            raise PwrapError(f"expected dim {VECTOR_DIM}, got {len(query)}")
        lit = "[" + ",".join(repr(float(x)) for x in query) + "]"

        async def fn(conn):
            return await conn.fetch(
                """
                SELECT doc_id,
                       metadata,
                       embedding <=> $1::vector AS distance
                FROM pwrap_embeddings
                WHERE collection = $2
                ORDER BY embedding <=> $1::vector
                LIMIT $3
                """,
                lit, self._collection, k,
            )

        rows = await self._client._run(fn)
        out = []
        for r in rows:
            meta = r["metadata"]
            if isinstance(meta, str):
                meta = json.loads(meta)
            out.append(Match(doc_id=r["doc_id"], metadata=meta, distance=float(r["distance"])))
        return out
