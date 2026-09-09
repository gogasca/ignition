"""Ships a finished trajectory out of the sandbox.

Ignition sandboxes take no inbound connections, so the rollout result leaves by
the harness dialing *out* to a collector (``COLLECTOR_URL``, reachable from GKE
pod egress) with a bearer token from ``secretRefs``. The PUT is idempotent and
keyed by the trajectory key, so a retry or an Ignition idempotency replay is
harmless.
"""

from __future__ import annotations

import time
import urllib.error
import urllib.request

from ..trajectory import Trajectory


def push(collector_url: str, token: str, traj: Trajectory, *, attempts: int = 5) -> bool:
    url = f"{collector_url.rstrip('/')}/trajectories/{traj.key}"
    body = traj.to_json().encode()
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"

    last_err: Exception | None = None
    for i in range(attempts):
        try:
            req = urllib.request.Request(url, data=body, headers=headers, method="PUT")
            with urllib.request.urlopen(req, timeout=30) as resp:
                if 200 <= resp.status < 300:
                    return True
        except (urllib.error.URLError, TimeoutError, OSError) as e:  # noqa: PERF203
            last_err = e
        time.sleep(min(2 ** i, 15))
    print(f"collector push failed after {attempts} attempts: {last_err}")
    return False
