#!/usr/bin/env python3
"""Cross-SDK conformance runner for the Python SDK.

Runs the scenarios listed for "py" in conformance/scenarios.json against a live
pwrapd and writes report-py.json, which conformance/check.py then gates on.

  PWRAP_CONTROL_URL      default http://localhost:8080
  PWRAP_BOOTSTRAP_TOKEN  default dev-admin
  PWRAP_REPORT_DIR       where to write report-py.json (default: cwd)
"""
from __future__ import annotations

import asyncio
import contextlib
import json
import math
import os
import pathlib
import sys
import uuid
from typing import AsyncIterator, Callable, Awaitable

import httpx

from pwrap import PwrapClient, VECTOR_DIM

CONTROL_URL = os.environ.get("PWRAP_CONTROL_URL", "http://localhost:8080")
ADMIN_TOKEN = os.environ.get("PWRAP_BOOTSTRAP_TOKEN", "dev-admin")
REPORT_DIR = pathlib.Path(os.environ.get("PWRAP_REPORT_DIR", "."))


def fake_embedding(seed: float) -> list[float]:
    """Mirrors the Go and TypeScript runners exactly, so all three are compared
    on identical vectors rather than merely similar ones."""
    return [math.sin(seed + i * 0.001) for i in range(VECTOR_DIM)]


# --- control plane -------------------------------------------------------------


@contextlib.asynccontextmanager
async def admin_client() -> AsyncIterator[httpx.AsyncClient]:
    async with httpx.AsyncClient(
        base_url=CONTROL_URL,
        headers={"Authorization": f"Bearer {ADMIN_TOKEN}"},
        timeout=60.0,
    ) as c:
        yield c


async def create_project(admin: httpx.AsyncClient, name: str) -> str:
    # Unique suffix so reruns against a live stack don't 409.
    r = await admin.post("/v1/projects", json={"name": f"{name}-{uuid.uuid4().hex[:8]}"})
    r.raise_for_status()
    return r.json()["id"]


async def issue_key(admin: httpx.AsyncClient, project_id: str) -> str:
    r = await admin.post(f"/v1/projects/{project_id}/keys", json={"name": "conformance"})
    r.raise_for_status()
    return r.json()["key"]


@contextlib.asynccontextmanager
async def provisioned(name: str) -> AsyncIterator[tuple[str, str, httpx.AsyncClient]]:
    """Yields (project_id, api_key, admin) with migrations applied; deletes on exit."""
    async with admin_client() as admin:
        project_id = await create_project(admin, name)
        try:
            r = await admin.post(f"/v1/projects/{project_id}/migrations")
            r.raise_for_status()
            api_key = await issue_key(admin, project_id)
            yield project_id, api_key, admin
        finally:
            with contextlib.suppress(Exception):
                await admin.delete(f"/v1/projects/{project_id}")


# --- scenarios -----------------------------------------------------------------


async def scenario_table_crud() -> None:
    async with provisioned("conf-table-crud") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            notes = c.table("notes")
            doc_id = await notes.insert({"title": "hello", "tags": ["a"]})

            got = await notes.get(doc_id)
            assert got is not None and got.data["title"] == "hello", f"title = {got}"

            rows = await notes.find({"tags": ["a"]})
            assert len(rows) == 1, f"find returned {len(rows)} rows, want 1"

            await notes.update(doc_id, {"pinned": True})
            got2 = await notes.get(doc_id)
            assert got2 is not None and got2.data["pinned"] is True

            n = await notes.count({"title": "hello"})
            assert n == 1, f"count = {n}, want 1"

            await notes.delete(doc_id)
            assert await notes.get(doc_id) is None, "get after delete returned a row"
            threw = False
            try:
                await notes.delete(doc_id)
            except Exception:
                threw = True
            assert threw, "second delete reported success, want failure"


async def scenario_table_batch() -> None:
    async with provisioned("conf-table-batch") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            notes = c.table("notes")
            ids = await notes.insert_many([{"title": "a"}, {"title": "b"}, {"title": "c"}])
            assert len(ids) == 3, f"got {len(ids)} ids, want 3"
            # Order must match the input, not insertion race order.
            for i, want in enumerate(["a", "b", "c"]):
                doc = await notes.get(ids[i])
                assert doc is not None and doc.data["title"] == want, (
                    f"ids[{i}] has title {doc.data['title'] if doc else None}, "
                    f"want {want} — order not preserved"
                )


async def scenario_vector_upsert_search() -> None:
    async with provisioned("conf-vector") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            vec = c.vector("notes")
            await vec.upsert("a", fake_embedding(0.0), {"name": "a"})
            await vec.upsert("b", fake_embedding(10.0), {"name": "b"})
            await vec.upsert("c", fake_embedding(20.0), {"name": "c"})

            matches = await vec.search(fake_embedding(0.01), k=3)
            assert len(matches) == 3, f"got {len(matches)} matches, want 3"
            assert matches[0].doc_id == "a", f"nearest = {matches[0].doc_id}, want a"
            for i in range(1, len(matches)):
                assert matches[i].distance >= matches[i - 1].distance, (
                    f"distances not ascending: {matches[i-1].distance} then {matches[i].distance}"
                )


async def scenario_vector_dim_validation() -> None:
    async with provisioned("conf-vector-dim") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            threw = False
            try:
                await c.vector("notes").upsert("short", [0.1, 0.2])
            except Exception:
                threw = True
            assert threw, "upsert with 2 dimensions succeeded, want a validation error"


async def scenario_queue_enqueue_stats() -> None:
    async with provisioned("conf-queue") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            q = c.queue()
            job_id = await q.enqueue(kind="embed", args={"doc_id": "x"})
            assert job_id > 0, f"job id = {job_id}, want positive"
            stats = await q.stats()
            assert stats.available >= 1, f"available = {stats.available}, want >= 1"


async def scenario_geo_radius_nearest() -> None:
    async with provisioned("conf-geo") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            geo = c.geo("places")
            await geo.insert_point(2.2945, 48.8584, {"name": "Eiffel"})
            await geo.insert_point(2.3499, 48.8530, {"name": "Notre-Dame"})
            await geo.insert_point(-73.9857, 40.7484, {"name": "Empire State"})

            near = await geo.within_radius(2.3522, 48.8566, 5000)
            names = sorted(f.metadata["name"] for f in near)
            assert names == ["Eiffel", "Notre-Dame"], (
                f"within 5km of Paris = {names}, want ['Eiffel', 'Notre-Dame']"
            )

            nearest = await geo.nearest(2.3522, 48.8566, 1)
            assert len(nearest) == 1, f"nearest returned {len(nearest)}, want 1"
            assert nearest[0].metadata["name"] in {"Eiffel", "Notre-Dame"}, (
                f"nearest to Paris = {nearest[0].metadata['name']}, want a Paris landmark"
            )


async def scenario_rest_token() -> None:
    async with provisioned("conf-rest-token") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            tok = await c.issue_rest_token(user_id="someone", ttl_seconds=60)
            assert tok.token.startswith("ey"), f"token {tok.token} does not look like a JWT"
            assert tok.url, "empty url"
            assert tok.role.startswith("p_"), f"role = {tok.role}, want a p_ tenant role"


async def scenario_schema_version_guard() -> None:
    # Deliberately skip `migrate apply` — connecting must fail loudly.
    async with admin_client() as admin:
        project_id = await create_project(admin, "conf-unmigrated")
        try:
            key = await issue_key(admin, project_id)
            err: Exception | None = None
            try:
                c = await PwrapClient.connect(api_key=key, control_url=CONTROL_URL)
                await c.close()
            except Exception as e:  # noqa: BLE001 — any failure mode is acceptable
                err = e
            assert err is not None, "connected to an unmigrated project, want an error"
            assert "migrat" in str(err).lower(), f"error {err!r} does not mention migrations"
        finally:
            with contextlib.suppress(Exception):
                await admin.delete(f"/v1/projects/{project_id}")


RLS_POLICY_SQL = """
ALTER TABLE pwrap_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE pwrap_documents FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS p ON pwrap_documents;
CREATE POLICY p ON pwrap_documents
    USING      (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id')
    WITH CHECK (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id');
"""


async def scenario_rls_with_user() -> None:
    async with provisioned("conf-rls") as (project_id, key, admin):
        r = await admin.post(
            f"/v1/projects/{project_id}/sql",
            content=RLS_POLICY_SQL,
            headers={"Content-Type": "application/sql"},
        )
        r.raise_for_status()

        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            alice, bob = c.with_user("alice"), c.with_user("bob")
            await alice.table("notes").insert({"user_id": "alice", "n": 1})
            await alice.table("notes").insert({"user_id": "alice", "n": 2})
            await bob.table("notes").insert({"user_id": "bob", "n": 99})

            for label, client, want in (("alice", alice, 2), ("bob", bob, 1), ("unscoped", c, 0)):
                rows = await client.table("notes").find()
                assert len(rows) == want, f"{label} saw {len(rows)} rows, want {want}"


async def scenario_realtime_subscribe() -> None:
    async with provisioned("conf-realtime") as (_, key, _a):
        async with await PwrapClient.connect(api_key=key, control_url=CONTROL_URL) as c:
            sub = await c.subscribe(table="pwrap_documents")
            try:
                # subscribe() returns only after the hello frame, so this insert
                # cannot race ahead of the subscription.
                await c.table("live").insert({"marker": "realtime"})
                event = await asyncio.wait_for(sub.__anext__(), timeout=30)
                assert event.op.upper() == "INSERT", f"op = {event.op}, want INSERT"
            finally:
                await sub.close()


SCENARIOS: dict[str, Callable[[], Awaitable[None]]] = {
    "table_crud": scenario_table_crud,
    "table_batch": scenario_table_batch,
    "vector_upsert_search": scenario_vector_upsert_search,
    "vector_dim_validation": scenario_vector_dim_validation,
    "queue_enqueue_stats": scenario_queue_enqueue_stats,
    "geo_radius_nearest": scenario_geo_radius_nearest,
    "rest_token": scenario_rest_token,
    "schema_version_guard": scenario_schema_version_guard,
    "rls_with_user": scenario_rls_with_user,
    "realtime_subscribe": scenario_realtime_subscribe,
}


async def main() -> int:
    results: dict[str, str] = {}
    failed = 0
    for sid, run in SCENARIOS.items():
        try:
            await run()
            results[sid] = "pass"
            print(f"ok   {sid}")
        except Exception as e:  # noqa: BLE001 — a scenario failure is data, not a crash
            results[sid] = f"fail: {e}"
            print(f"FAIL {sid}: {e}")
            failed += 1

    REPORT_DIR.mkdir(parents=True, exist_ok=True)
    (REPORT_DIR / "report-py.json").write_text(
        json.dumps({"sdk": "py", "results": results}, indent=2) + "\n"
    )
    total = len(SCENARIOS)
    print(f"\npy: {total - failed}/{total} scenarios passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
