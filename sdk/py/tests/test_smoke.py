"""End-to-end smoke test for the Python SDK.

Requires a live pwrapd at PWRAP_CONTROL_URL with PWRAP_BOOTSTRAP_TOKEN. Skipped
automatically when pwrapd isn't reachable.
"""
from __future__ import annotations

import math
import socket

import pytest

from pwrap import NotFoundError, PwrapClient, VECTOR_DIM


def _pwrapd_reachable() -> bool:
    try:
        with socket.create_connection(("localhost", 8080), timeout=1):
            return True
    except OSError:
        return False


pytestmark = pytest.mark.skipif(
    not _pwrapd_reachable(), reason="pwrapd not reachable at localhost:8080"
)


def _fake_embedding(seed: float) -> list[float]:
    # Deterministic 1536-dim vector keyed off `seed` — good enough to exercise HNSW.
    return [math.sin(seed + i * 0.001) for i in range(VECTOR_DIM)]


async def test_table_crud(project_key) -> None:
    _, key = project_key
    async with await PwrapClient.connect(api_key=key) as c:
        notes = c.table("notes")
        doc_id = await notes.insert({"title": "hello", "tags": ["a"]})
        got = await notes.get(doc_id)
        assert got is not None and got.data["title"] == "hello"

        rows = await notes.find({"tags": ["a"]})
        assert len(rows) == 1

        await notes.update(doc_id, {"pinned": True})
        got2 = await notes.get(doc_id)
        assert got2 is not None and got2.data["pinned"] is True

        assert await notes.count({"title": "hello"}) == 1

        await notes.delete(doc_id)
        assert await notes.get(doc_id) is None
        with pytest.raises(NotFoundError):
            await notes.delete(doc_id)


async def test_vector(project_key) -> None:
    _, key = project_key
    async with await PwrapClient.connect(api_key=key) as c:
        vec = c.vector("notes")
        await vec.upsert("a", _fake_embedding(0.0), {"name": "a"})
        await vec.upsert("b", _fake_embedding(10.0), {"name": "b"})
        await vec.upsert("c", _fake_embedding(20.0), {"name": "c"})

        # Query that's closest to "a" but with a small offset.
        matches = await vec.search(_fake_embedding(0.01), k=3)
        assert len(matches) == 3
        assert matches[0].doc_id == "a"
        # cosine distance is non-negative and the closest match should be tiny.
        assert matches[0].distance < matches[1].distance


async def test_queue_enqueue(project_key) -> None:
    _, key = project_key
    async with await PwrapClient.connect(api_key=key) as c:
        q = c.queue()
        job_id = await q.enqueue(kind="embed", args={"doc_id": "x"})
        assert job_id > 0
        stats = await q.stats()
        assert stats.available >= 1


async def test_geo(project_key) -> None:
    _, key = project_key
    async with await PwrapClient.connect(api_key=key) as c:
        geo = c.geo("places")
        await geo.insert_point(2.2945, 48.8584, {"name": "Eiffel"})
        await geo.insert_point(2.3499, 48.8530, {"name": "Notre-Dame"})
        await geo.insert_point(-73.9857, 40.7484, {"name": "Empire State"})

        # Within 5km of Paris center → 2 hits.
        near = await geo.within_radius(2.3522, 48.8566, 5000)
        names = sorted(f.metadata["name"] for f in near)
        assert names == ["Eiffel", "Notre-Dame"]

        # KNN: 1 nearest to Paris center is one of the Paris landmarks.
        nearest = await geo.nearest(2.3522, 48.8566, 1)
        assert nearest[0].metadata["name"] in {"Eiffel", "Notre-Dame"}


async def test_with_user_isolation(project_key, admin_client) -> None:
    """RLS through with_user works identically to the Go SDK's WithUser."""
    project_id, key = project_key

    # Enable RLS on pwrap_documents via the admin sql-apply endpoint.
    rls_sql = """
        ALTER TABLE pwrap_documents ENABLE ROW LEVEL SECURITY;
        ALTER TABLE pwrap_documents FORCE  ROW LEVEL SECURITY;
        DROP POLICY IF EXISTS p ON pwrap_documents;
        CREATE POLICY p ON pwrap_documents
            USING      (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id')
            WITH CHECK (data->>'user_id' = NULLIF(current_setting('request.jwt.claims', true), '')::jsonb->>'user_id');
    """
    r = await admin_client.post(
        f"/v1/projects/{project_id}/sql",
        content=rls_sql,
        headers={"Content-Type": "application/sql"},
    )
    r.raise_for_status()

    async with await PwrapClient.connect(api_key=key) as c:
        alice = c.with_user("alice")
        bob = c.with_user("bob")
        await alice.table("notes").insert({"user_id": "alice", "n": 1})
        await alice.table("notes").insert({"user_id": "alice", "n": 2})
        await bob.table("notes").insert({"user_id": "bob", "n": 99})

        assert len(await alice.table("notes").find()) == 2
        assert len(await bob.table("notes").find()) == 1
        # Unscoped client — policy denies, zero rows.
        assert len(await c.table("notes").find()) == 0


async def test_batch_apis(project_key) -> None:
    """Verify Table.insert_many / Vector.upsert_many / Geo.insert_point_many round-trip."""
    _, key = project_key
    async with await PwrapClient.connect(api_key=key) as c:
        notes = c.table("notes")
        ids = await notes.insert_many([
            {"title": "a", "n": 1},
            {"title": "b", "n": 2},
            {"title": "c", "n": 3},
        ])
        assert len(ids) == 3
        # Order preservation: WITH ORDINALITY in the SQL guarantees this.
        got_a = await notes.get(ids[0])
        assert got_a is not None and got_a.data["title"] == "a"

        vec = c.vector("notes")
        await vec.upsert_many([
            {"doc_id": "d1", "embedding": _fake_embedding(0.0), "metadata": {"i": 1}},
            {"doc_id": "d2", "embedding": _fake_embedding(10.0)},
            {"doc_id": "d3", "embedding": _fake_embedding(20.0), "metadata": {"i": 3}},
        ])
        matches = await vec.search(_fake_embedding(0.01), k=1)
        assert matches[0].doc_id == "d1"

        # upsert_many is idempotent — same doc_id overwrites
        await vec.upsert_many([
            {"doc_id": "d1", "embedding": _fake_embedding(100.0), "metadata": {"updated": True}},
        ])
        m = (await vec.search(_fake_embedding(100.0), k=1))[0]
        assert m.doc_id == "d1" and m.metadata.get("updated") is True

        geo = c.geo("places")
        geo_ids = await geo.insert_point_many([
            {"lng": 2.2945, "lat": 48.8584, "metadata": {"name": "Eiffel"}},
            {"lng": -73.9857, "lat": 40.7484, "metadata": {"name": "Empire State"}},
        ])
        assert len(geo_ids) == 2
        assert await geo.count() == 2

        # Mismatched embedding length must fail BEFORE touching the DB
        with pytest.raises(Exception):
            await vec.upsert_many([{"doc_id": "bad", "embedding": [0.1, 0.2]}])


async def test_rest_token(project_key) -> None:
    _, key = project_key
    async with await PwrapClient.connect(api_key=key) as c:
        tok = await c.issue_rest_token(user_id="someone", ttl_seconds=60)
        assert tok.token.startswith("ey")  # JWT
        assert tok.url
        assert tok.role.startswith("p_")
