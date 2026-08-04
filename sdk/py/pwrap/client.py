from __future__ import annotations

import json
from dataclasses import dataclass
from datetime import datetime
from typing import Any, AsyncIterator, Awaitable, Callable, Optional, TYPE_CHECKING

import asyncpg
import httpx

from .exceptions import PwrapError

if TYPE_CHECKING:  # pragma: no cover
    from .geo import Geo
    from .queue import Queue
    from .rest import RestToken
    from .subscribe import Subscription
    from .table import Table
    from .vector import Vector

DEFAULT_CONTROL_URL = "http://localhost:8080"
# Imported at use site to dodge the import cycle with __init__.py.
_SCHEMA_VERSION = "0001_init"


@dataclass
class _Connection:
    dsn: str
    schema: str
    expires_at: datetime


class PwrapClient:
    """Async pwrap client. Open via ``await PwrapClient.connect(api_key=...)``.

    The client owns an :class:`asyncpg.Pool` against the tenant schema and the
    project API key it bootstrapped from. Use it as an async context manager so
    the pool gets closed:

    .. code-block:: python

        async with await PwrapClient.connect(api_key="pwk_...") as c:
            ...
    """

    def __init__(
        self,
        *,
        pool: asyncpg.Pool,
        http: httpx.AsyncClient,
        control_url: str,
        api_key: str,
        schema: str,
        expires_at: datetime,
        user_id: Optional[str] = None,
    ) -> None:
        self._pool = pool
        self._http = http
        self._control_url = control_url.rstrip("/")
        self._api_key = api_key
        self._schema = schema
        self._expires_at = expires_at
        self._user_id = user_id

    # ---- lifecycle -------------------------------------------------------

    @classmethod
    async def connect(
        cls,
        *,
        api_key: str,
        control_url: str = DEFAULT_CONTROL_URL,
        http_client: Optional[httpx.AsyncClient] = None,
    ) -> "PwrapClient":
        """Exchange the API key at ``/v1/connection`` and open a pgvector-aware pool."""
        if not api_key:
            raise PwrapError("api_key is required")
        own_http = http_client is None
        http = http_client or httpx.AsyncClient(timeout=15.0)
        try:
            resp = await http.post(
                control_url.rstrip("/") + "/v1/connection",
                headers={"Authorization": f"Bearer {api_key}"},
            )
            resp.raise_for_status()
            body = resp.json()
            server_version = body.get("schema_version") or ""
            if not server_version:
                raise PwrapError(
                    "project has no migrations applied — run `pwrap migrate apply --project <id>`"
                )
            if server_version < _SCHEMA_VERSION:
                raise PwrapError(
                    f"tenant schema is {server_version!r} but this SDK requires "
                    f"{_SCHEMA_VERSION!r} — run `pwrap migrate apply --project <id>` to upgrade"
                )
            conn = _Connection(
                dsn=body["dsn"],
                schema=body["schema"],
                expires_at=datetime.fromisoformat(body["expires_at"].replace("Z", "+00:00")),
            )
        except Exception:
            if own_http:
                await http.aclose()
            raise

        pool = await asyncpg.create_pool(dsn=conn.dsn, min_size=1, max_size=10)
        # asyncpg returns vector/geometry columns as their server text representation
        # by default — that's fine for our SDK (we only emit them as strings).
        return cls(
            pool=pool,
            http=http,
            control_url=control_url,
            api_key=api_key,
            schema=conn.schema,
            expires_at=conn.expires_at,
        )

    async def close(self) -> None:
        await self._pool.close()
        await self._http.aclose()

    async def __aenter__(self) -> "PwrapClient":
        return self

    async def __aexit__(self, *exc) -> None:
        await self.close()

    # ---- introspection ---------------------------------------------------

    @property
    def schema(self) -> str:
        return self._schema

    @property
    def expires_at(self) -> datetime:
        return self._expires_at

    @property
    def pool(self) -> asyncpg.Pool:
        """Raw asyncpg pool — escape hatch for arbitrary SQL."""
        return self._pool

    @property
    def user_id(self) -> Optional[str]:
        return self._user_id

    # ---- RLS scoping -----------------------------------------------------

    def with_user(self, user_id: str) -> "PwrapClient":
        """Return a derived client whose Table operations execute in a tx with
        ``request.jwt.claims.user_id`` set. Identical contract to the Go SDK's
        ``WithUser`` and the TS SDK's planned equivalent.

        The derived client shares the underlying pool — don't call ``close`` on it.
        """
        return PwrapClient(
            pool=self._pool,
            http=self._http,
            control_url=self._control_url,
            api_key=self._api_key,
            schema=self._schema,
            expires_at=self._expires_at,
            user_id=user_id,
        )

    # ---- internal: run helper used by Table/Vector/Queue/Geo -------------

    async def _run(
        self,
        fn: Callable[[asyncpg.Connection], Awaitable[Any]],
    ) -> Any:
        if self._user_id is None:
            async with self._pool.acquire() as conn:
                return await fn(conn)
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                claims = json.dumps({"user_id": self._user_id})
                await conn.execute(
                    "SELECT set_config('request.jwt.claims', $1, true)", claims
                )
                return await fn(conn)

    # ---- factories -------------------------------------------------------

    def table(self, collection: str) -> "Table":
        from .table import Table
        return Table(self, collection)

    def vector(self, collection: str) -> "Vector":
        from .vector import Vector
        return Vector(self, collection)

    def queue(self) -> "Queue":
        from .queue import Queue
        return Queue(self)

    def geo(self, collection: str) -> "Geo":
        from .geo import Geo
        return Geo(self, collection)

    async def issue_rest_token(
        self,
        *,
        user_id: Optional[str] = None,
        ttl_seconds: Optional[int] = None,
    ) -> "RestToken":
        from .rest import issue_rest_token
        return await issue_rest_token(self, user_id=user_id, ttl_seconds=ttl_seconds)

    async def subscribe(
        self,
        *,
        table: Optional[str] = None,
        user_id: Optional[str] = None,
        timeout: float = 30.0,
    ) -> "Subscription":
        """Subscribe to change events; returns an async iterator of ChangeEvent.

        Mirrors the Go SDK's ``Subscribe`` and the TS SDK's ``subscribe``. Does
        not return until the server's hello frame arrives, so a write issued
        straight afterwards cannot race ahead of the subscription:

        .. code-block:: python

            sub = await c.subscribe(table="pwrap_documents")
            async for ev in sub:
                print(ev.op, ev.row_id)

        ``user_id`` narrows to changes carrying that jwt claim. Close with
        ``await sub.close()``, or use it as an async context manager.
        """
        from .subscribe import Subscription

        sub = Subscription(
            control_url=self._control_url,
            api_key=self._api_key,
            table=table,
            # Default to this client's RLS scope when it has one, so
            # `c.with_user("alice").subscribe()` does the expected thing.
            user_id=user_id if user_id is not None else self._user_id,
        )
        return await sub._start(timeout=timeout)

    # ---- internals exposed to sibling modules ----------------------------

    @property
    def _control_url_(self) -> str:
        return self._control_url

    @property
    def _api_key_(self) -> str:
        return self._api_key

    @property
    def _http_(self) -> httpx.AsyncClient:
        return self._http

    # iterator used internally to read rows; left here so type checkers see it
    async def _iter_rows(self, query: str, *args: Any) -> AsyncIterator[asyncpg.Record]:  # pragma: no cover
        async with self._pool.acquire() as conn:
            async for row in conn.cursor(query, *args):
                yield row
