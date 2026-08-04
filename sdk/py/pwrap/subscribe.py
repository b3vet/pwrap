"""Realtime change subscriptions over the pwrapd WebSocket endpoint.

Mirrors the Go SDK's ``Client.Subscribe`` and the TypeScript SDK's
``client.subscribe``: connect to ``/v1/subscribe``, wait for the server's hello
frame, then yield :class:`ChangeEvent` values until the subscription is closed.

    async with await PwrapClient.connect(api_key="pwk_...") as c:
        sub = await c.subscribe(table="pwrap_documents")
        async for ev in sub:
            print(ev.op, ev.row_id)

``subscribe()`` does not return until the handshake completes, so a write issued
immediately afterwards cannot race ahead of the subscription and be missed.
"""
from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass, field
from datetime import datetime
from typing import Any, AsyncIterator, Optional
from urllib.parse import urlencode, urlsplit, urlunsplit

from .exceptions import PwrapError

# Sentinel pushed onto the queue when the run loop ends, so __anext__ learns the
# stream is over without polling a flag.
_DONE = object()

# Matches the Go SDK: reconnect with exponential backoff, capped.
_INITIAL_BACKOFF = 1.0
_MAX_BACKOFF = 30.0
_HELLO_TIMEOUT = 5.0


@dataclass
class ChangeEvent:
    """One captured change. ``before``/``after`` are the row as JSON, or None."""

    log_id: int
    schema: str
    table: str
    op: str
    created_at: Optional[datetime] = None
    row_id: Optional[str] = None
    user_id: Optional[str] = None
    before: Optional[dict[str, Any]] = None
    after: Optional[dict[str, Any]] = None
    raw: dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def _from_json(cls, data: dict[str, Any]) -> "ChangeEvent":
        created = data.get("created_at")
        parsed: Optional[datetime] = None
        if isinstance(created, str):
            try:
                parsed = datetime.fromisoformat(created.replace("Z", "+00:00"))
            except ValueError:
                parsed = None
        return cls(
            log_id=int(data.get("log_id", 0)),
            schema=data.get("schema", ""),
            table=data.get("table", ""),
            op=data.get("op", ""),
            created_at=parsed,
            row_id=data.get("row_id"),
            user_id=data.get("user_id"),
            before=data.get("before"),
            after=data.get("after"),
            raw=data,
        )


class SubscriptionClosed(PwrapError):
    """Raised from iteration when the server closed the subscription for good."""


def _import_websockets():
    """Import the websockets client lazily.

    Lazy so that importing pwrap costs nothing for the majority of users who
    never subscribe, and so a missing/older install produces a message naming
    the fix rather than an opaque ImportError from deep in the call stack.
    """
    try:
        from websockets.asyncio.client import connect  # type: ignore[import-not-found]

        return connect
    except ImportError:  # pragma: no cover - depends on the installed version
        try:
            from websockets.client import connect  # type: ignore[import-not-found]

            return connect
        except ImportError as exc:
            raise PwrapError(
                "pwrap: realtime needs the `websockets` package "
                "(`pip install 'websockets>=12'`)."
            ) from exc


class Subscription:
    """An async iterator of :class:`ChangeEvent`.

    Reconnects with exponential backoff on transient failures. A close with
    status 1008 (policy violation) means the server rejected the credentials, so
    retrying would only loop; iteration raises :class:`SubscriptionClosed`.
    """

    def __init__(
        self,
        *,
        control_url: str,
        api_key: str,
        table: Optional[str] = None,
        user_id: Optional[str] = None,
    ) -> None:
        self._url = _build_url(control_url, api_key, table, user_id)
        self._queue: asyncio.Queue[Any] = asyncio.Queue()
        self._hello = asyncio.Event()
        self._closed = False
        self._error: Optional[BaseException] = None
        self._task: Optional[asyncio.Task[None]] = None

    # ---- lifecycle -------------------------------------------------------

    async def _start(self, timeout: float = 30.0) -> "Subscription":
        """Open the connection and wait for the hello frame."""
        self._task = asyncio.create_task(self._run_loop())
        hello = asyncio.create_task(self._hello.wait())
        done, _ = await asyncio.wait(
            {hello, self._task}, timeout=timeout, return_when=asyncio.FIRST_COMPLETED
        )
        if hello in done:
            return self

        # The run loop finished before the handshake, or nothing happened at all.
        hello.cancel()
        await self.close()
        if self._error is not None:
            raise self._error
        if self._task in done:
            raise PwrapError("pwrap: subscription ended before the server said hello")
        raise PwrapError(f"pwrap: timed out connecting to {self._url.split('?')[0]}")

    async def close(self) -> None:
        """Stop the subscription. Safe to call more than once."""
        self._closed = True
        task, self._task = self._task, None
        if task is not None and not task.done():
            task.cancel()
            try:
                await task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass
        # Unblock any consumer parked in __anext__.
        self._queue.put_nowait(_DONE)

    async def __aenter__(self) -> "Subscription":
        return self

    async def __aexit__(self, *_exc: Any) -> None:
        await self.close()

    # ---- iteration -------------------------------------------------------

    def __aiter__(self) -> AsyncIterator[ChangeEvent]:
        return self

    async def __anext__(self) -> ChangeEvent:
        item = await self._queue.get()
        if item is _DONE:
            # Put it back so further __anext__ calls also terminate rather than
            # hanging on an empty queue.
            self._queue.put_nowait(_DONE)
            if self._error is not None:
                raise SubscriptionClosed(str(self._error)) from self._error
            raise StopAsyncIteration
        return item

    # ---- internals -------------------------------------------------------

    async def _run_loop(self) -> None:
        backoff = _INITIAL_BACKOFF
        try:
            while not self._closed:
                try:
                    await self._run_once()
                    return  # server closed cleanly
                except asyncio.CancelledError:
                    raise
                except _PermanentError as exc:
                    self._error = exc
                    return
                except Exception:  # noqa: BLE001 — transient; reconnect
                    if self._closed:
                        return
                    await asyncio.sleep(backoff)
                    backoff = min(backoff * 2, _MAX_BACKOFF)
                else:
                    backoff = _INITIAL_BACKOFF
        finally:
            self._queue.put_nowait(_DONE)

    async def _run_once(self) -> None:
        connect = _import_websockets()
        async with connect(self._url) as ws:
            # First frame is the hello envelope; anything else means the server
            # is speaking a protocol this SDK doesn't know.
            try:
                await asyncio.wait_for(ws.recv(), timeout=_HELLO_TIMEOUT)
            except asyncio.TimeoutError as exc:
                raise PwrapError("pwrap: no hello frame from pwrapd") from exc
            self._hello.set()

            try:
                async for raw in ws:
                    event = _parse_event(raw)
                    if event is not None:
                        await self._queue.put(event)
            except Exception as exc:  # noqa: BLE001
                if _close_code(exc) == 1008:
                    # Policy violation — bad api key, unknown project. Retrying
                    # would just loop against the same rejection.
                    raise _PermanentError(str(exc)) from exc
                raise


class _PermanentError(PwrapError):
    """Internal marker: do not reconnect."""


def _close_code(exc: BaseException) -> Optional[int]:
    code = getattr(exc, "code", None)
    if isinstance(code, int):
        return code
    rcvd = getattr(exc, "rcvd", None)
    return getattr(rcvd, "code", None)


def _parse_event(raw: Any) -> Optional[ChangeEvent]:
    if isinstance(raw, (bytes, bytearray)):
        raw = raw.decode("utf-8", "replace")
    try:
        data = json.loads(raw)
    except (TypeError, ValueError):
        return None  # not JSON; ignore rather than kill the stream
    if not isinstance(data, dict) or "op" not in data:
        return None
    return ChangeEvent._from_json(data)


def _build_url(
    control_url: str, api_key: str, table: Optional[str], user_id: Optional[str]
) -> str:
    parts = urlsplit(control_url.rstrip("/"))
    scheme = {"http": "ws", "https": "wss"}.get(parts.scheme)
    if scheme is None:
        raise PwrapError(f"pwrap: unsupported control url scheme {parts.scheme!r}")
    # The api key travels as a query parameter because browsers can't set headers
    # on a WebSocket handshake; pwrapd accepts either. See SECURITY.md.
    query = {"api_key": api_key}
    if table:
        query["table"] = table
    if user_id:
        query["user_id"] = user_id
    return urlunsplit((scheme, parts.netloc, "/v1/subscribe", urlencode(query), ""))
