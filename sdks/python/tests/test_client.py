"""SDK tests against an in-process fake ignition-api (stdlib http.server) and,
for exec streaming, a fake ignition-gateway WebSocket."""

from __future__ import annotations

import json
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from ignition_sandbox import Client, NotFoundError, UnauthenticatedError

PROJECT = "prj_dev"


class FakeAPI(BaseHTTPRequestHandler):
    sandboxes: dict = {}
    procs: dict = {}
    hits: dict = {}
    gateway_url = ""

    def log_message(self, *a):  # silence
        pass

    # -- helpers -----------------------------------------------------
    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("X-Request-Id", "req_test")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _err(self, status, code):
        self._json(status, {"error": {"code": code, "message": code, "requestId": "req_test"}})

    def _read_body(self):
        n = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(n) or b"{}") if n else {}

    def _auth_ok(self):
        return self.headers.get("Authorization") == "Bearer t0ken"

    def _p(self):
        return self.path.split("?", 1)[0].replace(f"/v1/projects/{PROJECT}", "/v1", 1)

    # -- routing ---------------------------------------------------
    def do_GET(self):
        if not self._auth_ok():
            return self._err(401, "UNAUTHENTICATED")
        p = self._p()
        if p == "/v1/me":
            return self._json(200, {"subject": "alice", "email": "alice@acme.test", "kind": "user"})
        if p == "/v1/sandboxes":
            return self._json(200, {"sandboxes": list(self.sandboxes.values()), "nextPageToken": ""})
        if p.startswith("/v1/sandboxes/") and p.endswith(":watch"):
            return self._watch_sandbox(p[len("/v1/sandboxes/"):-len(":watch")])
        if p.startswith("/v1/sandboxes/") and "/processes/" in p:
            pid = p.rsplit("/", 1)[1]
            return self._json(200, self.procs.get(pid, {})) if pid in self.procs else self._err(404, "NOT_FOUND")
        if p.startswith("/v1/sandboxes/") and p.endswith("/processes"):
            return self._json(200, {"processes": list(self.procs.values())})
        if p.startswith("/v1/sandboxes/"):
            sid = p[len("/v1/sandboxes/"):]
            sb = self.sandboxes.get(sid)
            if not sb:
                return self._err(404, "NOT_FOUND")
            self.hits[sid] = self.hits.get(sid, 0) + 1
            if self.hits[sid] >= 2:
                sb["state"] = "READY"
            return self._json(200, sb)
        if p == "/v1/operations":
            return self._json(200, {"operations": [{"id": "op_1", "state": "SUCCEEDED", "kind": "CREATE_SANDBOX"}], "nextPageToken": ""})
        return self._err(404, "NOT_FOUND")

    def do_POST(self):
        if not self._auth_ok():
            return self._err(401, "UNAUTHENTICATED")
        p = self._p()
        if self.headers.get("Idempotency-Key", "") == "":
            return self._err(400, "INVALID_ARGUMENT")  # mutations must carry a key
        if p == "/v1/sandboxes":
            sid = "sbx_1"
            sb = {"id": sid, "projectId": PROJECT, "state": "CREATING", "imageId": "img_seed",
                  "operationId": "op_1"}
            self.sandboxes[sid] = sb
            return self._json(202, {"sandbox": sb, "operation": {"id": "op_1", "state": "PENDING", "kind": "CREATE_SANDBOX"}})
        if p.endswith(":terminate"):
            sid = p[len("/v1/sandboxes/"):-len(":terminate")]
            self.sandboxes[sid]["state"] = "FINISHED"
            return self._json(202, {"sandbox": self.sandboxes[sid], "operation": {"id": "op_2", "state": "SUCCEEDED"}})
        if p.endswith("/processes"):
            self._read_body()
            pr = {"id": "prc_1", "sandboxId": "sbx_1", "state": "RUNNING", "command": ["echo", "hi"]}
            self.procs["prc_1"] = pr
            return self._json(200, pr)
        if p.endswith(":attach"):
            return self._json(200, {
                "streamToken": "stok", "gatewayUrl": FakeAPI.gateway_url,
                "expireTime": "2030-01-01T00:00:00Z", "streamEpoch": 1,
            })
        if p.endswith(":signal") or p.endswith(":cancel"):
            self.procs["prc_1"]["state"] = "EXITED"
            self.procs["prc_1"]["exitCode"] = 0
            return self._json(200, self.procs["prc_1"])
        return self._err(404, "NOT_FOUND")

    def _watch_sandbox(self, sid):
        sb = self.sandboxes.get(sid)
        if not sb:
            return self._err(404, "NOT_FOUND")
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        # First snapshot, then flip to READY.
        self.wfile.write(f"id: 1\nevent: snapshot\ndata: {json.dumps(sb)}\n\n".encode())
        self.wfile.flush()
        sb["state"] = "READY"
        self.wfile.write(f"id: 2\nevent: snapshot\ndata: {json.dumps(sb)}\n\n".encode())
        self.wfile.flush()


class SDKTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        FakeAPI.sandboxes, FakeAPI.procs, FakeAPI.hits = {}, {}, {}
        cls.srv = ThreadingHTTPServer(("127.0.0.1", 0), FakeAPI)
        cls.t = threading.Thread(target=cls.srv.serve_forever, daemon=True)
        cls.t.start()
        cls.base = f"http://127.0.0.1:{cls.srv.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()

    def client(self, **kw):
        return Client(server=self.base, token="t0ken", project=PROJECT, **kw)

    def test_me(self):
        self.assertEqual(self.client().me()["subject"], "alice")

    def test_auth_error(self):
        with self.assertRaises(UnauthenticatedError):
            Client(server=self.base, token="bad", project=PROJECT).me()

    def test_not_found(self):
        with self.assertRaises(NotFoundError):
            self.client().sandboxes.get("sbx_missing")

    def test_create_list_terminate(self):
        c = self.client()
        sb = c.sandboxes.create("img_seed", accelerator="NONE", cpu_milli=1000, memory_mib=2048)
        self.assertEqual(sb.state, "CREATING")
        self.assertEqual(sb.id, "sbx_1")
        ids = [s.id for s in c.sandboxes.list()]
        self.assertIn("sbx_1", ids)
        sb.terminate()
        self.assertTrue(sb.is_terminal)

    def test_wait_ready_via_watch(self):
        c = self.client()
        sb = c.sandboxes.create("img_seed", accelerator="NONE")
        sb.wait_ready(timeout=5)
        self.assertTrue(sb.is_ready)

    def test_mutations_send_idempotency_key(self):
        # FakeAPI 400s a POST without a key; a successful create proves we send one.
        sb = self.client().sandboxes.create("img_seed")
        self.assertEqual(sb.id, "sbx_1")

    def test_process_lifecycle(self):
        c = self.client()
        sb = c.sandboxes.create("img_seed")
        proc = sb.exec(["echo", "hi"])
        self.assertEqual(proc.state, "RUNNING")
        proc.cancel()
        self.assertTrue(proc.is_terminal)
        self.assertEqual(proc.exit_code, 0)


if __name__ == "__main__":
    unittest.main()
