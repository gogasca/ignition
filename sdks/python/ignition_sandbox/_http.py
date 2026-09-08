"""Minimal HTTP transport over the standard library.

The SDK deliberately has no runtime dependencies: this wraps ``urllib.request``
with the small amount of behavior the Ignition API needs — bearer auth, an
``Idempotency-Key`` on every mutation, stable error mapping, and a line reader
for the Server-Sent-Events ``:watch`` streams.
"""

from __future__ import annotations

import json
import os
import uuid
from typing import Any, Iterator
from urllib import error as urlerror
from urllib import request as urlrequest

from .errors import IgnitionError, from_response

_USER_AGENT = "ignition-sandbox-python/0.1"


class Transport:
    def __init__(self, server: str, token: str, project: str, timeout: float) -> None:
        if not server:
            raise IgnitionError(
                "no server configured: pass server=... or set IGNITION_SERVER"
            )
        self.server = server.rstrip("/")
        self.token = token
        self.project = project
        self.timeout = timeout

    # -- path helpers ------------------------------------------------------
    def _url(self, path: str) -> str:
        return self.server + path

    def project_path(self, suffix: str) -> str:
        if not self.project:
            raise IgnitionError(
                "no project configured: pass project=... or set IGNITION_PROJECT"
            )
        return f"/v1/projects/{self.project}{suffix}"

    # -- requests --------------------------------------------------------
    def request(
        self,
        method: str,
        path: str,
        body: Any | None = None,
        *,
        idempotent: bool = False,
        idempotency_key: str | None = None,
        query: dict[str, str] | None = None,
    ) -> Any:
        url = self._url(path)
        if query:
            pairs = "&".join(f"{k}={v}" for k, v in query.items() if v not in (None, ""))
            if pairs:
                url += "?" + pairs

        data = None
        headers = {"Accept": "application/json", "User-Agent": _USER_AGENT}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        if idempotent:
            headers["Idempotency-Key"] = idempotency_key or ("isdk-" + uuid.uuid4().hex)

        req = urlrequest.Request(url, data=data, method=method, headers=headers)
        try:
            with urlrequest.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
                if not raw:
                    return None
                return json.loads(raw)
        except urlerror.HTTPError as exc:  # non-2xx
            raw = exc.read()
            rid = exc.headers.get("X-Request-Id", "") if exc.headers else ""
            raise from_response(exc.code, raw, rid) from None
        except urlerror.URLError as exc:
            raise IgnitionError(f"{method} {path}: {exc.reason}") from None

    def get(self, path: str, **kw: Any) -> Any:
        return self.request("GET", path, **kw)

    def post(self, path: str, body: Any | None = None, **kw: Any) -> Any:
        return self.request("POST", path, body, **kw)

    # -- SSE ------------------------------------------------------------
    def sse(self, path: str, last_event_id: str = "") -> Iterator[dict]:
        """Yield each JSON snapshot from an SSE ``:watch`` stream.

        The connection is closed when the caller stops iterating.
        """
        url = self._url(path)
        headers = {"Accept": "text/event-stream", "User-Agent": _USER_AGENT}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        if last_event_id:
            headers["Last-Event-ID"] = last_event_id
        req = urlrequest.Request(url, method="GET", headers=headers)
        try:
            resp = urlrequest.urlopen(req, timeout=None)
        except urlerror.HTTPError as exc:
            raw = exc.read()
            rid = exc.headers.get("X-Request-Id", "") if exc.headers else ""
            raise from_response(exc.code, raw, rid) from None
        with resp:
            for line in resp:
                text = line.decode("utf-8", "replace").rstrip("\n")
                if text.startswith("data: "):
                    payload = text[6:]
                    try:
                        yield json.loads(payload)
                    except json.JSONDecodeError:
                        continue


def env_default(explicit: str | None, *names: str) -> str:
    if explicit:
        return explicit
    for n in names:
        v = os.environ.get(n)
        if v:
            return v
    return ""
