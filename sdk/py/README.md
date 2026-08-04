# pwrap — Python SDK

Async Python SDK for [pwrap](https://github.com/b3vet/pwrap).

```python
import asyncio
from pwrap import PwrapClient

async def main() -> None:
    async with await PwrapClient.connect(api_key="pwk_...") as c:
        notes = c.table("notes")
        doc_id = await notes.insert({"title": "hello", "tags": ["a"]})
        docs = await notes.find({"tags": ["a"]})
        print(docs)

asyncio.run(main())
```

## Install

```bash
pip install pwrap
```

Mirrors the Go and TypeScript SDKs: `Table` (JSONB collections), `Vector` (pgvector
search), `Queue` (insert-only), `Geo` (PostGIS), `Rest` (JWT exchange for PostgREST),
`subscribe()` for realtime change events, and `with_user(user_id)` for RLS-scoped
queries.

## Realtime

```python
sub = await c.subscribe(table="pwrap_documents")
async for ev in sub:
    print(ev.op, ev.row_id, ev.after)
```

`subscribe()` returns only after the server's hello frame, so a write issued
immediately afterwards can't race ahead of the subscription. It reconnects with
exponential backoff on transient failures, and stops on an auth rejection rather
than looping. Close with `await sub.close()`, or use it as an async context
manager.

## License

Apache 2.0.
