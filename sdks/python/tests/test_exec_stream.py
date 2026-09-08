"""Exec streaming test: the SDK's built-in WebSocket client (`_ws`) against a
fake ignition-gateway that speaks the execframe JSON protocol."""

from __future__ import annotations

import asyncio
import base64
import io
import json
import threading
import unittest

try:
    import websockets
except ImportError:  # pragma: no cover
    websockets = None

from ignition_sandbox.client import _stream_exec


class FakeGateway:
    """Echoes stdin to stdout, then sends a control/exit frame."""

    def __init__(self):
        self.loop = asyncio.new_event_loop()
        self.port = None
        self._ready = threading.Event()
        self._server = None
        self._thread = threading.Thread(target=self._run, daemon=True)

    def start(self):
        self._thread.start()
        self._ready.wait(5)

    def stop(self):
        async def _shutdown():
            if self._server is not None:
                self._server.close()
                await self._server.wait_closed()
            self.loop.stop()

        self.loop.call_soon_threadsafe(lambda: self.loop.create_task(_shutdown()))
        self._thread.join(timeout=3)

    def _run(self):
        asyncio.set_event_loop(self.loop)

        async def handler(ws):
            got = bytearray()
            async for msg in ws:
                frame = json.loads(msg)
                if frame.get("channel") == "stdin" and frame.get("kind") == "data":
                    got += base64.b64decode(frame["payload"])
                elif frame.get("kind") == "eof":
                    break
            await ws.send(json.dumps({
                "channel": "stdout", "kind": "data",
                "payload": base64.b64encode(bytes(got) or b"hello\n").decode(),
            }))
            await ws.send(json.dumps({"channel": "control", "kind": "exit", "exitCode": 7}))
            await ws.close()

        async def main():
            self._server = await websockets.serve(handler, "127.0.0.1", 0)
            self.port = self._server.sockets[0].getsockname()[1]
            self._ready.set()
            await asyncio.Future()

        try:
            self.loop.run_until_complete(main())
        except (RuntimeError, asyncio.CancelledError):
            pass


@unittest.skipIf(websockets is None, "websockets not installed")
class ExecStreamTest(unittest.TestCase):
    def test_roundtrip_exit_code(self):
        gw = FakeGateway()
        gw.start()
        try:
            out, err = io.BytesIO(), io.BytesIO()
            code, sig = _stream_exec(
                f"http://127.0.0.1:{gw.port}", "tok", "prc_1",
                b"ping\n", out, err,
            )
            self.assertEqual(code, 7)
            self.assertEqual(out.getvalue(), b"ping\n")
        finally:
            gw.stop()

    def test_default_output_when_no_stdin(self):
        gw = FakeGateway()
        gw.start()
        try:
            out, err = io.BytesIO(), io.BytesIO()
            code, _ = _stream_exec(
                f"http://127.0.0.1:{gw.port}", "tok", "prc_1", None, out, err,
            )
            self.assertEqual(code, 7)
            self.assertEqual(out.getvalue(), b"hello\n")
        finally:
            gw.stop()


if __name__ == "__main__":
    unittest.main()
