"""Synchronous client for the Ignition control plane.

Covers the shipped surface: sandbox lifecycle, the process control plane,
operations, ``:watch`` streams, and exec streaming through ``ignition-gateway``.
"""

from __future__ import annotations

import io
import json
import sys
import time
from typing import Any, BinaryIO, Iterator

from ._http import Transport, env_default
from ._ws import WebSocket, WSClosed
from .errors import IgnitionError, StreamError, TimeoutError
from .models import OperationModel, ProcessModel, SandboxModel

DEFAULT_TIMEOUT = 30.0


class ExecResult:
    """Outcome of :meth:`Sandbox.run`."""

    def __init__(
        self,
        process_id: str,
        exit_code: int | None,
        signal: str = "",
        *,
        stdout: bytes = b"",
        stderr: bytes = b"",
    ) -> None:
        self.process_id = process_id
        self.exit_code = exit_code
        self.signal = signal
        # Populated only when run(..., capture=True). The polling fallback cannot
        # capture output, so these stay empty there.
        self.stdout = stdout
        self.stderr = stderr

    @property
    def ok(self) -> bool:
        return self.exit_code == 0

    def __repr__(self) -> str:
        return f"ExecResult(process_id={self.process_id!r}, exit_code={self.exit_code}, signal={self.signal!r})"


class _Tee:
    """Fan writes out to several binary sinks (skips ``None``)."""

    def __init__(self, *sinks: "BinaryIO | None") -> None:
        self._sinks = [s for s in sinks if s is not None]

    def write(self, data: bytes) -> int:
        for s in self._sinks:
            s.write(data)
        return len(data)

    def flush(self) -> None:
        for s in self._sinks:
            try:
                s.flush()
            except Exception:  # noqa: BLE001
                pass


class Client:
    def __init__(
        self,
        server: str | None = None,
        token: str | None = None,
        project: str | None = None,
        *,
        timeout: float = DEFAULT_TIMEOUT,
    ) -> None:
        self._t = Transport(
            server=env_default(server, "IGNITION_SERVER"),
            token=env_default(token, "IGNITION_TOKEN"),
            project=env_default(project, "IGNITION_PROJECT"),
            timeout=timeout,
        )
        self.sandboxes = Sandboxes(self)
        self.operations = Operations(self)

    # -- context manager (symmetry with the docs; nothing to close) --------
    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc: object) -> None:
        return None

    @property
    def project(self) -> str:
        return self._t.project

    def me(self) -> dict:
        """Return the authenticated principal (also verifies the token)."""
        return self._t.get("/v1/me")

    def default_runtime(self) -> dict:
        return self._t.get(self._t.project_path("/runtimes/default"))


class Sandboxes:
    def __init__(self, client: Client) -> None:
        self._c = client
        self._t = client._t

    def create(
        self,
        image: str,
        *,
        command: list[str] | None = None,
        args: list[str] | None = None,
        working_directory: str | None = None,
        native_entrypoint: bool = False,
        accelerator: str | None = None,
        accelerator_count: int | None = None,
        cpu_milli: int | None = None,
        memory_mib: int | None = None,
        internet: bool = False,
        env: dict[str, str] | None = None,
        secret_refs: list[dict] | None = None,
        labels: dict[str, str] | None = None,
        region: str | None = None,
        startup_seconds: int | None = None,
        maximum_runtime_seconds: int | None = None,
        idle_seconds: int | None = None,
        termination_grace_seconds: int | None = None,
        name: str | None = None,
        idempotency_key: str | None = None,
        wait: bool = False,
        wait_timeout: float = 120.0,
    ) -> "Sandbox":
        body: dict[str, Any] = {"imageId": image}
        if name:
            body["name"] = name
        if command is not None:
            body["command"] = command
        if args is not None:
            body["args"] = args
        if working_directory:
            body["workingDirectory"] = working_directory
        if native_entrypoint:
            body["nativeEntrypoint"] = True
        if env:
            body["environment"] = env
        if secret_refs:
            body["secretRefs"] = secret_refs
        if labels:
            body["labels"] = labels

        resources: dict[str, Any] = {}
        if cpu_milli is not None:
            resources["cpuMilli"] = cpu_milli
        if memory_mib is not None:
            resources["memoryMiB"] = memory_mib
        if accelerator is not None:
            acc: dict[str, Any] = {"type": accelerator}
            if accelerator_count is not None:
                acc["count"] = accelerator_count
            elif accelerator not in ("NONE", None):
                acc["count"] = 1
            resources["accelerator"] = acc
        if resources:
            body["resources"] = resources

        if region:
            body["placement"] = {"region": region}

        timeouts: dict[str, Any] = {}
        for key, val in (
            ("startupSeconds", startup_seconds),
            ("maximumRuntimeSeconds", maximum_runtime_seconds),
            ("idleSeconds", idle_seconds),
            ("terminationGraceSeconds", termination_grace_seconds),
        ):
            if val is not None:
                timeouts[key] = val
        if timeouts:
            body["timeouts"] = timeouts

        body["network"] = {"internetAccess": "ENABLED" if internet else "DISABLED"}

        resp = self._t.post(
            self._t.project_path("/sandboxes"),
            body,
            idempotent=True,
            idempotency_key=idempotency_key,
        )
        sb = Sandbox(self._c, resp["sandbox"])
        if wait:
            sb.wait_ready(timeout=wait_timeout)
        return sb

    def get(self, sandbox_id: str) -> "Sandbox":
        return Sandbox(self._c, self._t.get(self._t.project_path(f"/sandboxes/{sandbox_id}")))

    def list(self, *, page_size: int | None = None) -> Iterator["Sandbox"]:
        token = ""
        while True:
            q = {}
            if page_size:
                q["pageSize"] = str(page_size)
            if token:
                q["pageToken"] = token
            resp = self._t.get(self._t.project_path("/sandboxes"), query=q or None)
            for item in resp.get("sandboxes") or []:
                yield Sandbox(self._c, item)
            token = resp.get("nextPageToken") or ""
            if not token:
                return


class Sandbox(SandboxModel):
    def __init__(self, client: Client, raw: dict) -> None:
        super().__init__(raw)
        self._c = client
        self._t = client._t

    def _path(self, suffix: str = "") -> str:
        return self._t.project_path(f"/sandboxes/{self.id}{suffix}")

    def refresh(self) -> "Sandbox":
        self.raw = self._t.get(self._path())
        return self

    def watch(self, *, last_event_id: str = "") -> Iterator["Sandbox"]:
        for snap in self._t.sse(self._path(":watch"), last_event_id):
            self.raw = snap
            yield self

    def wait_ready(self, *, timeout: float = 120.0) -> "Sandbox":
        deadline = time.monotonic() + timeout
        for _ in self._poll_or_watch(self._path(":watch"), self.refresh, deadline):
            if self.is_ready:
                return self
            if self.is_terminal:
                raise IgnitionError(
                    f"sandbox {self.id} became {self.state} ({self.state_reason}) before READY"
                )
        raise TimeoutError(f"sandbox {self.id} not READY within {timeout}s")

    def terminate(self, *, wait: bool = False, wait_timeout: float = 60.0) -> "Sandbox":
        resp = self._t.post(self._path(":terminate"), {}, idempotent=True)
        self.raw = resp.get("sandbox", self.raw)
        if wait:
            deadline = time.monotonic() + wait_timeout
            for _ in self._poll_or_watch(self._path(":watch"), self.refresh, deadline):
                if self.is_terminal:
                    return self
            raise TimeoutError(f"sandbox {self.id} not terminal within {wait_timeout}s")
        return self

    # -- processes -----------------------------------------------------
    @property
    def processes(self) -> "Processes":
        return Processes(self._c, self.id)

    def exec(
        self,
        command: list[str],
        *,
        env: dict[str, str] | None = None,
        working_directory: str | None = None,
        pty: bool = False,
        pty_rows: int = 0,
        pty_cols: int = 0,
        idempotency_key: str | None = None,
    ) -> "Process":
        """Create a process and return a handle. Does not stream output."""
        return self.processes.create(
            command,
            env=env,
            working_directory=working_directory,
            pty=pty,
            pty_rows=pty_rows,
            pty_cols=pty_cols,
            idempotency_key=idempotency_key,
        )

    def run(
        self,
        command: list[str],
        *,
        env: dict[str, str] | None = None,
        working_directory: str | None = None,
        stdin: bytes | BinaryIO | None = None,
        stdout: BinaryIO | None = None,
        stderr: BinaryIO | None = None,
        timeout: float | None = None,
        stream: bool = True,
        capture: bool = False,
    ) -> ExecResult:
        """Create a process, stream its stdio through ``ignition-gateway``, and
        return its exit code. Falls back to polling when no gateway is
        configured or ``stream=False``.

        With ``capture=True`` the streamed stdout/stderr bytes are also collected
        onto ``ExecResult.stdout`` / ``.stderr`` (in addition to any ``stdout`` /
        ``stderr`` sink you pass). The polling fallback has no output to capture,
        so those stay empty there.
        """
        proc = self.exec(command, env=env, working_directory=working_directory)
        cap_out = io.BytesIO() if capture else None
        cap_err = io.BytesIO() if capture else None
        if capture:
            out: BinaryIO = _Tee(stdout, cap_out)  # type: ignore[assignment]
            err: BinaryIO = _Tee(stderr, cap_err)  # type: ignore[assignment]
        else:
            out = stdout if stdout is not None else sys.stdout.buffer
            err = stderr if stderr is not None else sys.stderr.buffer

        if stream:
            attach = self._t.post(proc._path(":attach"), {}, idempotent=True)
            if attach.get("gatewayUrl") and attach.get("streamToken"):
                try:
                    code, sig = _stream_exec(
                        attach["gatewayUrl"], attach["streamToken"], proc.id, stdin, out, err
                    )
                    return ExecResult(
                        proc.id, code, sig,
                        stdout=cap_out.getvalue() if cap_out else b"",
                        stderr=cap_err.getvalue() if cap_err else b"",
                    )
                except StreamError:
                    # Gateway not reachable from here — fall through to polling.
                    pass

        # Polling fallback: wait for terminal state. Captured output is not
        # exposed by the API outside the gateway stream, so return status only.
        proc.wait(timeout=timeout)
        return ExecResult(proc.id, proc.exit_code, proc.signal)

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc: object) -> None:
        return None

    # -- shared poll/watch driver ------------------------------------
    def _poll_or_watch(self, watch_path: str, refresh, deadline: float) -> Iterator[None]:
        """Yield once per observation until the deadline. Uses the SSE stream
        when the server supports it, otherwise a 1s poll."""
        try:
            for snap in self._t.sse(watch_path):
                self.raw = snap
                yield None
                if time.monotonic() > deadline:
                    return
        except IgnitionError:
            pass
        while time.monotonic() < deadline:
            refresh()
            yield None
            time.sleep(1.0)


class Processes:
    def __init__(self, client: Client, sandbox_id: str) -> None:
        self._c = client
        self._t = client._t
        self.sandbox_id = sandbox_id

    def _base(self) -> str:
        return self._t.project_path(f"/sandboxes/{self.sandbox_id}/processes")

    def create(
        self,
        command: list[str],
        *,
        env: dict[str, str] | None = None,
        working_directory: str | None = None,
        pty: bool = False,
        pty_rows: int = 0,
        pty_cols: int = 0,
        idempotency_key: str | None = None,
    ) -> "Process":
        body: dict[str, Any] = {"command": command}
        if env:
            body["environment"] = env
        if working_directory:
            body["workingDirectory"] = working_directory
        if pty:
            body["pty"] = True
            if pty_rows:
                body["ptyRows"] = pty_rows
            if pty_cols:
                body["ptyCols"] = pty_cols
        raw = self._t.post(self._base(), body, idempotent=True, idempotency_key=idempotency_key)
        return Process(self._c, self.sandbox_id, raw)

    def get(self, process_id: str) -> "Process":
        return Process(self._c, self.sandbox_id, self._t.get(f"{self._base()}/{process_id}"))

    def list(self) -> list["Process"]:
        resp = self._t.get(self._base())
        return [Process(self._c, self.sandbox_id, p) for p in resp.get("processes") or []]


class Process(ProcessModel):
    def __init__(self, client: Client, sandbox_id: str, raw: dict) -> None:
        super().__init__(raw)
        self._c = client
        self._t = client._t
        self.sandbox_id = sandbox_id

    def _path(self, suffix: str = "") -> str:
        return self._t.project_path(
            f"/sandboxes/{self.sandbox_id}/processes/{self.id}{suffix}"
        )

    def refresh(self) -> "Process":
        self.raw = self._t.get(self._path())
        return self

    def signal(self, sig: str) -> "Process":
        self.raw = self._t.post(self._path(":signal"), {"signal": sig}, idempotent=True)
        return self

    def cancel(self) -> "Process":
        self.raw = self._t.post(self._path(":cancel"), {}, idempotent=True)
        return self

    def attach_token(self) -> dict:
        """Mint a fresh exec stream token: ``{streamToken, gatewayUrl, ...}``."""
        return self._t.post(self._path(":attach"), {}, idempotent=True)

    def stream(
        self,
        *,
        stdin: bytes | BinaryIO | None = None,
        stdout: BinaryIO | None = None,
        stderr: BinaryIO | None = None,
    ) -> ExecResult:
        at = self.attach_token()
        if not at.get("gatewayUrl") or not at.get("streamToken"):
            raise StreamError("no gateway configured for this deployment")
        out = stdout if stdout is not None else sys.stdout.buffer
        err = stderr if stderr is not None else sys.stderr.buffer
        code, sig = _stream_exec(at["gatewayUrl"], at["streamToken"], self.id, stdin, out, err)
        return ExecResult(self.id, code, sig)

    def wait(self, *, timeout: float | None = None) -> int | None:
        deadline = time.monotonic() + (timeout if timeout is not None else 1e9)
        while time.monotonic() < deadline:
            self.refresh()
            if self.is_terminal:
                return self.exit_code
            time.sleep(0.5)
        raise TimeoutError(f"process {self.id} not terminal within {timeout}s")


class Operations:
    def __init__(self, client: Client) -> None:
        self._c = client
        self._t = client._t

    def get(self, operation_id: str) -> "Operation":
        return Operation(self._c, self._t.get(self._t.project_path(f"/operations/{operation_id}")))

    def list(self) -> list["Operation"]:
        resp = self._t.get(self._t.project_path("/operations"))
        return [Operation(self._c, o) for o in resp.get("operations") or []]


class Operation(OperationModel):
    def __init__(self, client: Client, raw: dict) -> None:
        super().__init__(raw)
        self._c = client
        self._t = client._t

    def _path(self, suffix: str = "") -> str:
        return self._t.project_path(f"/operations/{self.id}{suffix}")

    def refresh(self) -> "Operation":
        self.raw = self._t.get(self._path())
        return self

    def cancel(self) -> "Operation":
        self.raw = self._t.post(self._path(":cancel"), {}, idempotent=True)
        return self

    def watch(self, *, last_event_id: str = "") -> Iterator["Operation"]:
        for snap in self._t.sse(self._path(":watch"), last_event_id):
            self.raw = snap
            yield self

    def wait(self, *, timeout: float = 300.0) -> "Operation":
        deadline = time.monotonic() + timeout
        try:
            for snap in self._t.sse(self._path(":watch")):
                self.raw = snap
                if self.is_terminal:
                    return self
                if time.monotonic() > deadline:
                    break
        except IgnitionError:
            pass
        while time.monotonic() < deadline:
            self.refresh()
            if self.is_terminal:
                return self
            time.sleep(1.0)
        raise TimeoutError(f"operation {self.id} not terminal within {timeout}s")


# -- exec stream --------------------------------------------------------
def _stream_exec(
    gateway_url: str,
    token: str,
    process_id: str,
    stdin: bytes | BinaryIO | None,
    stdout: BinaryIO,
    stderr: BinaryIO,
) -> tuple[int | None, str]:
    import base64
    import threading

    ws_url = gateway_url.replace("https://", "wss://", 1).replace("http://", "ws://", 1)
    ws_url = ws_url.rstrip("/") + "/v1/attach?token=" + token
    try:
        ws = WebSocket(ws_url)
    except Exception as exc:  # noqa: BLE001
        raise StreamError(f"gateway attach failed: {exc}") from None

    def pump_stdin() -> None:
        try:
            if stdin is None:
                pass
            elif isinstance(stdin, (bytes, bytearray)):
                if stdin:
                    ws.send_text(json.dumps({
                        "channel": "stdin", "kind": "data",
                        "payload": base64.b64encode(bytes(stdin)).decode(),
                    }))
            else:
                while True:
                    chunk = stdin.read(32768)
                    if not chunk:
                        break
                    ws.send_text(json.dumps({
                        "channel": "stdin", "kind": "data",
                        "payload": base64.b64encode(chunk).decode(),
                    }))
            ws.send_text(json.dumps({"channel": "stdin", "kind": "eof"}))
        except Exception:  # noqa: BLE001 - stdin pump failures should not crash the reader
            pass

    t = threading.Thread(target=pump_stdin, daemon=True)
    t.start()

    exit_code: int | None = None
    signal = ""
    try:
        while True:
            frame = json.loads(ws.recv_text())
            ch, kind = frame.get("channel"), frame.get("kind")
            payload = frame.get("payload")
            data = base64.b64decode(payload) if payload else b""
            if ch == "stdout" and kind == "data":
                stdout.write(data)
                stdout.flush()
            elif ch == "stderr" and kind == "data":
                stderr.write(data)
                stderr.flush()
            elif ch == "control" and kind == "exit":
                exit_code = frame.get("exitCode")
                signal = frame.get("signal", "")
                break
            elif kind == "error":
                raise StreamError(f"stream error: {frame.get('reason')}")
    except WSClosed:
        pass
    finally:
        ws.close()
    return exit_code, signal
