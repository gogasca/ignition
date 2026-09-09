"""A trajectory collector: the small HTTP service the in-sandbox harness dials
out to (topology A), since Ignition sandboxes accept no inbound connections.

    PUT  /trajectories/{key}   bearer-guarded, body is a Trajectory JSON
    GET  /trajectories/{key}   returns it (404 until it arrives)
    GET  /healthz

Every accepted trajectory is also appended to ``{out_dir}/{run_id}.jsonl``.
For a real training run this is a standing service with a stable URL reachable
from GKE pod egress; here it also runs in-process (:class:`Collector`) for tests
and single-box demos.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from ..trajectory import Trajectory


class _Store:
    def __init__(self, out_dir: str) -> None:
        self.lock = threading.Lock()
        self.by_key: dict[str, Trajectory] = {}
        self.out_dir = Path(out_dir)
        self.out_dir.mkdir(parents=True, exist_ok=True)

    def put(self, traj: Trajectory) -> None:
        with self.lock:
            first = traj.key not in self.by_key
            self.by_key[traj.key] = traj
            if first:
                path = self.out_dir / f"{traj.run_id}.jsonl"
                with open(path, "a", encoding="utf-8") as fh:
                    fh.write(traj.to_json() + "\n")

    def get(self, key: str) -> Trajectory | None:
        with self.lock:
            return self.by_key.get(key)


def _handler(store: _Store, token: str):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):  # quiet
            pass

        def _send(self, status: int, obj: dict) -> None:
            body = json.dumps(obj).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _auth_ok(self) -> bool:
            return not token or self.headers.get("Authorization") == f"Bearer {token}"

        def do_GET(self):
            if self.path == "/healthz":
                return self._send(200, {"ok": True})
            if not self._auth_ok():
                return self._send(401, {"error": "unauthorized"})
            if self.path.startswith("/trajectories/"):
                key = self.path[len("/trajectories/"):]
                traj = store.get(key)
                if traj is None:
                    return self._send(404, {"error": "not found", "key": key})
                return self._send(200, traj.to_dict())
            return self._send(404, {"error": "no route"})

        def do_PUT(self):
            if not self._auth_ok():
                return self._send(401, {"error": "unauthorized"})
            if not self.path.startswith("/trajectories/"):
                return self._send(404, {"error": "no route"})
            n = int(self.headers.get("Content-Length", "0"))
            try:
                traj = Trajectory.from_json(self.rfile.read(n).decode())
            except (ValueError, KeyError) as e:
                return self._send(400, {"error": f"bad trajectory: {e}"})
            store.put(traj)
            return self._send(200, {"ok": True, "key": traj.key})

    return Handler


class Collector:
    """In-process collector for tests and single-box runs."""

    def __init__(self, out_dir: str = "trajectories", token: str = "", port: int = 0) -> None:
        self.store = _Store(out_dir)
        self.token = token
        self._srv = ThreadingHTTPServer(("127.0.0.1", port), _handler(self.store, token))
        self._thread = threading.Thread(target=self._srv.serve_forever, daemon=True)

    @property
    def port(self) -> int:
        return self._srv.server_address[1]

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def get(self, key: str) -> Trajectory | None:
        return self.store.get(key)

    def wait_for(self, key: str, timeout: float = 30.0, poll: float = 0.25) -> Trajectory | None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            t = self.store.get(key)
            if t is not None:
                return t
            time.sleep(poll)
        return None

    def __enter__(self) -> "Collector":
        self._thread.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._srv.shutdown()
        self._srv.server_close()


def main(argv: list[str]) -> int:
    import argparse

    p = argparse.ArgumentParser(description="Run the trajectory collector.")
    p.add_argument("--port", type=int, default=8900)
    p.add_argument("--out-dir", default="trajectories")
    p.add_argument("--token", default="")
    a = p.parse_args(argv[1:])
    srv = ThreadingHTTPServer(("0.0.0.0", a.port), _handler(_Store(a.out_dir), a.token))
    print(f"collector listening on :{a.port}, writing {a.out_dir}/<run_id>.jsonl")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(main(sys.argv))
