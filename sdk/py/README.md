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
and `with_user(user_id)` for RLS-scoped queries.

## License

Apache 2.0.
