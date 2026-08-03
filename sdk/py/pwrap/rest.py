from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Optional

from .exceptions import PwrapError, RestDisabledError


@dataclass
class RestToken:
    token: str
    url: str
    role: str
    expires_at: datetime


async def issue_rest_token(
    client,
    *,
    user_id: Optional[str] = None,
    ttl_seconds: Optional[int] = None,
) -> RestToken:
    """Exchange the SDK's API key for a PostgREST-compatible JWT."""
    body: dict = {}
    if user_id:
        body["user_id"] = user_id
    if ttl_seconds:
        body["ttl_seconds"] = ttl_seconds
    resp = await client._http_.post(
        client._control_url_ + "/v1/rest/token",
        headers={
            "Authorization": f"Bearer {client._api_key_}",
            "Content-Type": "application/json",
        },
        json=body,
    )
    if resp.status_code == 503:
        raise RestDisabledError("pwrapd has no JWT secret configured")
    if resp.status_code >= 400:
        raise PwrapError(f"/v1/rest/token {resp.status_code}: {resp.text.strip()}")
    data = resp.json()
    return RestToken(
        token=data["token"],
        url=data["url"],
        role=data["role"],
        expires_at=datetime.fromisoformat(data["expires_at"].replace("Z", "+00:00")),
    )
