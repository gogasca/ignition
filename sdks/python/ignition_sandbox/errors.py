"""Exception types raised by the Ignition SDK."""

from __future__ import annotations


class IgnitionError(Exception):
    """Base class for every SDK error."""


class APIError(IgnitionError):
    """A non-2xx response from ignition-api.

    ``code`` is the stable machine code (e.g. ``QUOTA_EXCEEDED``); ``status`` is
    the HTTP status; ``request_id`` ties a report back to a server log line.
    """

    def __init__(
        self,
        status: int,
        code: str = "",
        message: str = "",
        request_id: str = "",
        retryable: bool = False,
        details: dict | None = None,
    ) -> None:
        self.status = status
        self.code = code or f"HTTP_{status}"
        self.message = message or ""
        self.request_id = request_id
        self.retryable = retryable
        self.details = details or {}
        text = f"{self.code}: {self.message or status}"
        if request_id:
            text += f" (request {request_id})"
        super().__init__(text)


class NotFoundError(APIError):
    """404 — the resource does not exist or is in another project."""


class PermissionDeniedError(APIError):
    """403 — the caller lacks the required permission."""


class UnauthenticatedError(APIError):
    """401 — missing or invalid credential."""


class ConflictError(APIError):
    """409 — idempotency-key reuse or a resource-state conflict."""


class TimeoutError(IgnitionError):  # noqa: A001 - deliberately shadows builtins in this namespace
    """A wait_* helper exceeded its deadline."""


class StreamError(IgnitionError):
    """The exec attach stream failed."""


def from_response(status: int, body: bytes, request_id: str) -> APIError:
    import json

    code = message = ""
    retryable = False
    details: dict = {}
    try:
        payload = json.loads(body or b"{}")
        err = payload.get("error", payload)
        code = err.get("code", "")
        message = err.get("message", "")
        retryable = bool(err.get("retryable", False))
        details = err.get("details", {}) or {}
        request_id = err.get("requestId", request_id)
    except Exception:  # noqa: BLE001 - a non-JSON error body is still an error
        pass

    cls = {
        401: UnauthenticatedError,
        403: PermissionDeniedError,
        404: NotFoundError,
        409: ConflictError,
    }.get(status, APIError)
    return cls(status, code, message, request_id, retryable, details)
