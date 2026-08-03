from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, Optional

from .exceptions import NotFoundError


@dataclass
class Feature:
    id: str
    geometry: dict[str, Any]
    metadata: dict[str, Any]
    created_at: datetime
    updated_at: datetime
    distance_meters: float = 0.0


class Geo:
    """PostGIS-backed feature collection. Geometries stored as EPSG:4326."""

    def __init__(self, client, collection: str) -> None:
        self._client = client
        self._collection = collection

    async def insert_point_many(
        self,
        points: list[dict[str, Any]],
    ) -> list[str]:
        """Bulk insert of points. Each entry is ``{"lng": float, "lat": float, "metadata"?: dict}``.

        Returns generated ids in input order. Single round-trip via `unnest()`.
        """
        if not points:
            return []
        lngs: list[float] = []
        lats: list[float] = []
        metas: list[str] = []
        for p in points:
            lngs.append(float(p["lng"]))
            lats.append(float(p["lat"]))
            metas.append(json.dumps(p.get("metadata") or {}))

        async def fn(conn):
            return await conn.fetch(
                """
                INSERT INTO pwrap_geo (collection, geom, metadata)
                SELECT $1, ST_SetSRID(ST_MakePoint(lng, lat), 4326), meta::jsonb
                  FROM unnest($2::float8[], $3::float8[], $4::text[]) WITH ORDINALITY AS u(lng, lat, meta, ord)
                 ORDER BY ord
                RETURNING id
                """,
                self._collection, lngs, lats, metas,
            )

        rows = await self._client._run(fn)
        return [str(r["id"]) for r in rows]

    async def insert_point(
        self,
        lng: float,
        lat: float,
        metadata: Optional[dict[str, Any]] = None,
    ) -> str:
        meta = json.dumps(metadata or {})

        async def fn(conn):
            return await conn.fetchval(
                """
                INSERT INTO pwrap_geo (collection, geom, metadata)
                VALUES ($1, ST_SetSRID(ST_MakePoint($2, $3), 4326), $4::jsonb)
                RETURNING id
                """,
                self._collection, float(lng), float(lat), meta,
            )

        row_id = await self._client._run(fn)
        return str(row_id)

    async def insert(self, geometry: dict[str, Any], metadata: Optional[dict[str, Any]] = None) -> str:
        meta = json.dumps(metadata or {})
        geom = json.dumps(geometry)

        async def fn(conn):
            return await conn.fetchval(
                """
                INSERT INTO pwrap_geo (collection, geom, metadata)
                VALUES ($1, ST_SetSRID(ST_GeomFromGeoJSON($2), 4326), $3::jsonb)
                RETURNING id
                """,
                self._collection, geom, meta,
            )

        row_id = await self._client._run(fn)
        return str(row_id)

    async def get(self, feature_id: str) -> Optional[Feature]:
        async def fn(conn):
            return await conn.fetchrow(
                """
                SELECT id,
                       ST_AsGeoJSON(geom)::text AS geometry,
                       metadata::text          AS metadata,
                       created_at, updated_at
                FROM pwrap_geo
                WHERE collection = $1 AND id = $2
                """,
                self._collection, feature_id,
            )

        row = await self._client._run(fn)
        if row is None:
            return None
        return _row_to_feature(row)

    async def delete(self, feature_id: str) -> None:
        async def fn(conn):
            tag = await conn.execute(
                "DELETE FROM pwrap_geo WHERE collection = $1 AND id = $2",
                self._collection, feature_id,
            )
            return int(tag.rsplit(" ", 1)[-1])

        affected = await self._client._run(fn)
        if affected == 0:
            raise NotFoundError(f"feature {feature_id!r} not found")

    async def within_radius(
        self, lng: float, lat: float, meters: float, limit: int = 100,
    ) -> list[Feature]:
        async def fn(conn):
            return await conn.fetch(
                """
                SELECT id,
                       ST_AsGeoJSON(geom)::text AS geometry,
                       metadata::text          AS metadata,
                       created_at, updated_at,
                       ST_Distance(
                           geom::geography,
                           ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
                       ) AS distance_meters
                FROM pwrap_geo
                WHERE collection = $3
                  AND ST_DWithin(
                      geom::geography,
                      ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography,
                      $4
                  )
                ORDER BY distance_meters
                LIMIT $5
                """,
                float(lng), float(lat), self._collection, float(meters), limit,
            )

        rows = await self._client._run(fn)
        return [_row_to_feature(r, with_distance=True) for r in rows]

    async def within_bbox(
        self, min_lng: float, min_lat: float, max_lng: float, max_lat: float, limit: int = 100,
    ) -> list[Feature]:
        async def fn(conn):
            return await conn.fetch(
                """
                SELECT id,
                       ST_AsGeoJSON(geom)::text AS geometry,
                       metadata::text          AS metadata,
                       created_at, updated_at
                FROM pwrap_geo
                WHERE collection = $1
                  AND geom && ST_MakeEnvelope($2, $3, $4, $5, 4326)
                LIMIT $6
                """,
                self._collection, float(min_lng), float(min_lat),
                float(max_lng), float(max_lat), limit,
            )

        rows = await self._client._run(fn)
        return [_row_to_feature(r) for r in rows]

    async def nearest(self, lng: float, lat: float, k: int = 10) -> list[Feature]:
        async def fn(conn):
            return await conn.fetch(
                """
                SELECT id,
                       ST_AsGeoJSON(geom)::text AS geometry,
                       metadata::text          AS metadata,
                       created_at, updated_at,
                       ST_Distance(
                           geom::geography,
                           ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography
                       ) AS distance_meters
                FROM pwrap_geo
                WHERE collection = $3
                ORDER BY geom <-> ST_SetSRID(ST_MakePoint($1, $2), 4326)
                LIMIT $4
                """,
                float(lng), float(lat), self._collection, k,
            )

        rows = await self._client._run(fn)
        return [_row_to_feature(r, with_distance=True) for r in rows]

    async def count(self) -> int:
        async def fn(conn):
            return await conn.fetchval(
                "SELECT COUNT(*) FROM pwrap_geo WHERE collection = $1",
                self._collection,
            )

        return int(await self._client._run(fn))


def _row_to_feature(row, *, with_distance: bool = False) -> Feature:
    geom = row["geometry"]
    if isinstance(geom, str):
        geom = json.loads(geom)
    meta = row["metadata"]
    if isinstance(meta, str):
        meta = json.loads(meta)
    f = Feature(
        id=str(row["id"]),
        geometry=geom,
        metadata=meta,
        created_at=row["created_at"],
        updated_at=row["updated_at"],
    )
    if with_distance and "distance_meters" in row:
        f.distance_meters = float(row["distance_meters"])
    return f


_ = field
