"""Test harness for the Python SDK.

Each test gets its own pwrap project (created via the admin API), with migrations
applied and an API key minted. The project is deleted in teardown so test runs are
isolated.

Skips the whole test module if pwrapd isn't reachable.
"""
from __future__ import annotations

import os
import socket
import uuid

import httpx
import pytest

CONTROL_URL = os.environ.get("PWRAP_CONTROL_URL", "http://localhost:8080")
ADMIN_TOKEN = os.environ.get("PWRAP_BOOTSTRAP_TOKEN", "dev-admin")


def _pwrapd_reachable() -> bool:
    try:
        with socket.create_connection(("localhost", 8080), timeout=1):
            return True
    except OSError:
        return False


pytestmark_skip_if_no_pwrapd = pytest.mark.skipif(
    not _pwrapd_reachable(), reason="pwrapd not reachable at localhost:8080"
)


@pytest.fixture
async def admin_client() -> httpx.AsyncClient:
    async with httpx.AsyncClient(
        base_url=CONTROL_URL,
        headers={"Authorization": f"Bearer {ADMIN_TOKEN}"},
        timeout=30.0,
    ) as c:
        yield c


@pytest.fixture
async def project_key(admin_client: httpx.AsyncClient) -> tuple[str, str]:
    """Create a project, apply migrations, issue a key; return (project_id, api_key)."""
    name = f"pytest-{uuid.uuid4().hex[:10]}"
    r = await admin_client.post("/v1/projects", json={"name": name})
    r.raise_for_status()
    project_id = r.json()["id"]
    try:
        r = await admin_client.post(f"/v1/projects/{project_id}/migrations")
        r.raise_for_status()
        r = await admin_client.post(
            f"/v1/projects/{project_id}/keys", json={"name": "pytest"}
        )
        r.raise_for_status()
        api_key = r.json()["key"]
        yield project_id, api_key
    finally:
        await admin_client.delete(f"/v1/projects/{project_id}")
