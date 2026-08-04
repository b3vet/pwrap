"""pwrap — Python SDK for the pwrap Postgres-backed backend."""

from .client import PwrapClient
from .exceptions import PwrapError, NotFoundError, RestDisabledError
from .table import Document, Table
from .vector import Match, Vector, VECTOR_DIM
from .queue import Queue, Stats
from .geo import Feature, Geo
from .rest import RestToken
from .subscribe import ChangeEvent, Subscription, SubscriptionClosed

__all__ = [
    "PwrapClient",
    "PwrapError",
    "NotFoundError",
    "RestDisabledError",
    "Document",
    "Table",
    "Match",
    "Vector",
    "VECTOR_DIM",
    "Queue",
    "Stats",
    "Feature",
    "Geo",
    "RestToken",
    "ChangeEvent",
    "Subscription",
    "SubscriptionClosed",
]

SCHEMA_VERSION = "0001_init"
