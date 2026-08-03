class PwrapError(Exception):
    """Base error for the pwrap SDK."""


class NotFoundError(PwrapError):
    """Raised when get/update/delete targets a missing row."""


class RestDisabledError(PwrapError):
    """Raised when /v1/rest/token returns 503 — pwrapd has no JWT secret configured."""
