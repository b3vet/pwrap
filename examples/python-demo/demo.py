"""End-to-end pwrap demo in Python — mirrors examples/todo-plus but uses the Python SDK.

Run from the repo root with the python venv that has pwrap installed (see sdk/py).

    $ /tmp/pwrap-venv/bin/python examples/python-demo/demo.py

Provisions a fresh project, uses each pillar of the SDK, then deletes the project.
"""
from __future__ import annotations

import asyncio
import math
import os
import time
import uuid
from typing import cast

import httpx

from pwrap import PwrapClient, VECTOR_DIM

CONTROL_URL = os.environ.get("PWRAP_CONTROL_URL", "http://localhost:8080")
ADMIN_TOKEN = os.environ["PWRAP_BOOTSTRAP_TOKEN"]


def fake_embedding(text: str) -> list[float]:
    h = sum(ord(c) * (i + 1) for i, c in enumerate(text.lower()))
    return [math.sin(h / 1000 + i * 0.001) for i in range(VECTOR_DIM)]


async def main() -> None:
    async with httpx.AsyncClient(
        base_url=CONTROL_URL,
        headers={"Authorization": f"Bearer {ADMIN_TOKEN}"},
        timeout=30.0,
    ) as admin:
        # Provision a fresh project.
        r = await admin.post("/v1/projects", json={"name": f"py-demo-{int(time.time())}"})
        r.raise_for_status()
        project = r.json()
        project_id = project["id"]
        try:
            (await admin.post(f"/v1/projects/{project_id}/migrations")).raise_for_status()
            r = await admin.post(f"/v1/projects/{project_id}/keys", json={"name": "demo"})
            r.raise_for_status()
            api_key = r.json()["key"]
            await _run_demo(api_key)
        finally:
            await admin.delete(f"/v1/projects/{project_id}")


async def _run_demo(api_key: str) -> None:
    async with await PwrapClient.connect(api_key=api_key) as c:
        print(f"[sdk] connected to schema {c.schema}")

        # --- Table -------------------------------------------------------
        notes = c.table("notes")
        titles = [
            "Buy groceries",
            "Ship Python SDK",
            "Walk the dog",
            "Write blog post on pgvector",
        ]
        ids: list[str] = []
        for t in titles:
            doc_id = await notes.insert({"title": t, "status": "todo"})
            ids.append(doc_id)
        print(f"[table] inserted {len(ids)} notes; count(status=todo) = {await notes.count({'status': 'todo'})}")

        # --- Vector ------------------------------------------------------
        v = c.vector("notes")
        for doc_id, title in zip(ids, titles, strict=True):
            await v.upsert(doc_id, fake_embedding(title), {"title": title})
        matches = await v.search(fake_embedding("Postgres vector search"), k=3)
        print("[vector] top-3 cosine matches for 'Postgres vector search':")
        for m in matches:
            print(f"  distance={m.distance:.4f}  {m.metadata['title']}")

        # --- Geo ---------------------------------------------------------
        geo = c.geo("places")
        await geo.insert_point(2.2945, 48.8584, {"name": "Eiffel Tower"})
        await geo.insert_point(-0.1246, 51.5007, {"name": "Big Ben"})
        await geo.insert_point(139.7454, 35.6586, {"name": "Tokyo Tower"})
        near_paris = await geo.within_radius(2.3522, 48.8566, 5000)
        print(f"[geo] within 5km of Paris: {[f.metadata['name'] for f in near_paris]}")

        # --- Queue -------------------------------------------------------
        q = c.queue()
        await q.enqueue(kind="embed", args={"doc_id": ids[0]})
        await q.enqueue(kind="embed", args={"doc_id": ids[1]})
        print(f"[queue] stats: {await q.stats()}")

        # --- REST JWT ----------------------------------------------------
        tok = await c.issue_rest_token(user_id=str(uuid.uuid4()))
        print(f"[rest] JWT issued for role={tok.role} until {tok.expires_at.isoformat()}")
        cast(int, len(tok.token))  # keep mypy/ruff happy


if __name__ == "__main__":
    asyncio.run(main())
