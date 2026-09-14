"""Materialized-view helper backed by the ``pwrap_matviews`` registry."""
from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass
from datetime import datetime
from typing import Any, Optional

from .exceptions import NotFoundError, PwrapError

# Matview names are interpolated into DDL, which cannot be parameterised, so the
# accepted alphabet is narrowed to what a plain Postgres identifier can hold.
# Exotic quoted names are rejected rather than escaped.
_IDENT = re.compile(r"^[A-Za-z0-9_]{1,63}$")


@dataclass
class MatviewInfo:
    """Registry row for a matview."""

    name: str
    definition: str
    checksum: str
    last_refresh_at: Optional[datetime]
    last_error: Optional[str]
    enabled: bool
    created_at: datetime
    updated_at: datetime


class Matview:
    """Handle over a named materialized view.

    Scheduling is the caller's job — :meth:`refresh` is the primitive to run on a
    cron, a worker, or on demand.
    """

    def __init__(self, client, name: str) -> None:
        self._client = client
        self._name = name

    @property
    def name(self) -> str:
        return self._name

    async def register(self, definition: str) -> None:
        """Ensure the matview exists with this definition and record it.

        ``definition`` is the SQL *after* the ``AS`` — e.g.
        ``SELECT data->>'topic' AS topic, count(*) FROM pwrap_documents GROUP BY 1``.
        A matview already registered under a different definition is dropped and
        recreated. The new matview is created ``WITH NO DATA``; the first
        :meth:`refresh` populates it.
        """
        _validate_ident(self._name)
        definition = definition.strip()
        if not definition:
            raise PwrapError("matview: definition is empty")
        checksum = hashlib.sha256(definition.encode()).hexdigest()

        async def fn(conn):
            existing = await conn.fetchval(
                "SELECT checksum FROM pwrap_matviews WHERE name = $1", self._name
            )
            # A matching checksum means the view already reflects this definition;
            # recreating it would throw away populated data for nothing.
            if existing == checksum:
                return
            quoted = _quote_ident(self._name)
            if existing is not None:
                await conn.execute(f"DROP MATERIALIZED VIEW IF EXISTS {quoted}")
            await conn.execute(
                f"CREATE MATERIALIZED VIEW IF NOT EXISTS {quoted} AS {definition} WITH NO DATA"
            )
            # Whoever runs CREATE owns the result, and clients connect as a
            # credential that expires within the hour. Left alone the matview
            # outlives its owner: REFRESH requires ownership, so the next
            # credential — including this client's own, after its hourly
            # rotation — is refused with "must be owner". The schema's owner is
            # the project's tenant role, which outlives every credential minted
            # for it and which every ephemeral role inherits.
            owner = await conn.fetchval(
                "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = current_schema()"
            )
            await conn.execute(
                f"ALTER MATERIALIZED VIEW {quoted} OWNER TO {_quote_ident(owner)}"
            )
            # No unique index is synthesised: only the caller knows which columns
            # are unique, and REFRESH CONCURRENTLY needs one.
            await conn.execute(
                """
                INSERT INTO pwrap_matviews (name, definition, checksum)
                VALUES ($1, $2, $3)
                ON CONFLICT (name) DO UPDATE
                    SET definition = EXCLUDED.definition,
                        checksum   = EXCLUDED.checksum,
                        updated_at = now()
                """,
                self._name, definition, checksum,
            )

        await self._client._run(fn)

    async def refresh(self) -> None:
        """``REFRESH MATERIALIZED VIEW``. Holds an ACCESS EXCLUSIVE lock throughout."""
        await self._refresh(concurrent=False)

    async def refresh_concurrent(self) -> None:
        """``REFRESH MATERIALIZED VIEW CONCURRENTLY`` — reads continue during the refresh.

        Postgres requires the matview to carry a unique index and to have been
        populated at least once, so call :meth:`refresh` first and create the
        index yourself.
        """
        await self._refresh(concurrent=True)

    async def _refresh(self, *, concurrent: bool) -> None:
        _validate_ident(self._name)
        stmt = "REFRESH MATERIALIZED VIEW {}{}".format(
            "CONCURRENTLY " if concurrent else "", _quote_ident(self._name)
        )

        async def fn(conn):
            failure: Optional[BaseException] = None
            try:
                await conn.execute(stmt)
            except Exception as e:  # noqa: BLE001 — recorded, then re-raised below
                failure = e
            # The registry is updated either way: success stamps last_refresh_at
            # and clears the error, failure keeps the old timestamp and records why.
            message = str(failure) if failure is not None else None
            await conn.execute(
                """
                UPDATE pwrap_matviews
                   SET last_refresh_at = CASE WHEN $2::text IS NULL THEN now() ELSE last_refresh_at END,
                       last_error      = $2,
                       updated_at      = now()
                 WHERE name = $1
                """,
                self._name, message,
            )
            if failure is not None:
                raise PwrapError(f"matview refresh: {message}") from failure

        await self._client._run(fn)

    async def drop(self) -> None:
        """Drop the matview and forget its registry row."""
        _validate_ident(self._name)

        async def fn(conn):
            await conn.execute(
                f"DROP MATERIALIZED VIEW IF EXISTS {_quote_ident(self._name)}"
            )
            await conn.execute("DELETE FROM pwrap_matviews WHERE name = $1", self._name)

        await self._client._run(fn)

    async def info(self) -> MatviewInfo:
        """The current registry row. Raises ``NotFoundError`` if never registered."""

        async def fn(conn):
            return await conn.fetchrow(
                """
                SELECT name, definition, checksum, last_refresh_at, last_error,
                       enabled, created_at, updated_at
                FROM pwrap_matviews WHERE name = $1
                """,
                self._name,
            )

        row: Any = await self._client._run(fn)
        if row is None:
            raise NotFoundError(f"matview {self._name} is not registered")
        return MatviewInfo(
            name=row["name"],
            definition=row["definition"],
            checksum=row["checksum"],
            last_refresh_at=row["last_refresh_at"],
            last_error=row["last_error"],
            enabled=row["enabled"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )


def _validate_ident(s: str) -> None:
    if not _IDENT.match(s or ""):
        raise PwrapError(
            "matview: name must be 1..63 characters of [A-Za-z0-9_], got "
            f"{s!r}"
        )


def _quote_ident(s: str) -> str:
    return '"' + s.replace('"', '""') + '"'
