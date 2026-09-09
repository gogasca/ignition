"""In-process stand-ins for the Ignition control plane, so the driver can be
tested with no GCP, no Docker, and no LLM.

- ``FakeClient``      — topology A. ``create`` spins a thread that plays the
  in-sandbox harness locally (real ``swe_mini.harness`` + ``OraclePolicy``) and
  PUTs the resulting trajectory to the collector URL from the sandbox env, then
  drives the sandbox to a terminal state. Exercises the real controller retry /
  teardown logic and the real collector HTTP path.
- ``LocalExecClient`` — topology B. ``sb.run`` executes the command locally with
  ``/env`` and ``/scratch`` path prefixes rewritten, so the driver-side agent
  loop runs against real ``cp`` / ``cat`` / ``pytest`` / ``verify``.
"""

from __future__ import annotations

import io
import os
import shutil
import subprocess
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

from ignition_sandbox.client import ExecResult
from ignition_sandbox.errors import ConflictError

from swe_mini.harness import agent, verify
from swe_mini.policy import OraclePolicy, RandomEditPolicy
from swe_mini.trajectory import Trajectory, make_key

REPO_EXAMPLE = Path(__file__).resolve().parent.parent  # examples/agentic-rl
TASKS = REPO_EXAMPLE / "swe_mini" / "tasks"


# --------------------------------------------------------------------------- #
# Topology A
# --------------------------------------------------------------------------- #
class _FakeProc:
    def __init__(self) -> None:
        self.raw = {"id": "proc_fake", "state": "EXITED", "exitCode": 0}

    def refresh(self):
        return self

    @property
    def exit_code(self):
        return 0


class _FakeProcs:
    def list(self):
        return [_FakeProc()]


class FakeSandbox:
    def __init__(self, sid: str) -> None:
        self.id = sid
        self.raw = {"id": sid, "state": "CREATING"}
        self.processes = _FakeProcs()
        self.terminated = False

    @property
    def state(self):
        return self.raw["state"]

    @property
    def is_terminal(self):
        return self.raw["state"] in ("FINISHED", "FAILED")

    @property
    def is_ready(self):
        return self.raw["state"] == "READY"

    def watch(self):
        for _ in range(200):
            yield self
            if self.is_terminal:
                return
            time.sleep(0.02)

    def wait_ready(self, timeout: float = 0):
        return self

    def terminate(self, wait: bool = False):
        self.terminated = True
        self.raw["state"] = "FINISHED"
        return self


class _FakeSandboxes:
    def __init__(self, client: "FakeClient") -> None:
        self._c = client

    def create(self, image, **kw):
        key = kw.get("idempotency_key", "")
        self._c.created.append(kw)

        if key in self._c.conflict_keys:
            raise ConflictError(409, "CONFLICT", "idempotency replay")

        sid = f"sb_{len(self._c.created)}"
        sb = FakeSandbox(sid)
        self._c.sandboxes_created.append(sb)
        sb.raw["state"] = "READY"
        threading.Thread(
            target=self._c._play_harness, args=(sb, kw), daemon=True
        ).start()
        return sb


class FakeClient:
    """Topology-A fake. ``policy_factory(task_dir, sample) -> Policy``."""

    def __init__(self, policy_factory=None, harness_delay: float = 0.05) -> None:
        self.created: list[dict] = []
        self.sandboxes_created: list[FakeSandbox] = []
        self.conflict_keys: set[str] = set()
        self.harness_delay = harness_delay
        self.sandboxes = _FakeSandboxes(self)
        self._policy_factory = policy_factory or (
            lambda task_dir, sample: OraclePolicy(task_dir)
            if sample % 2 == 0
            else RandomEditPolicy(seed=sample)
        )

    def _play_harness(self, sb: FakeSandbox, kw: dict) -> None:
        time.sleep(self.harness_delay)
        env = kw.get("env") or {}
        task_id = env["TASK_ID"]
        work = Path(tempfile.mkdtemp())
        src = TASKS / task_id
        for item in src.iterdir():
            if item.name == "meta.json":
                continue
            (shutil.copytree if item.is_dir() else shutil.copy2)(item, work / item.name)

        sample = int(env.get("SAMPLE", "0"))
        policy = self._policy_factory(src, sample)
        turns = agent.run_agent(work, (src / "PROMPT.md").read_text(), policy, max_turns=8)
        result = verify.score(work)
        traj = Trajectory(
            key=make_key(env["RUN_ID"], task_id, int(env["POLICY_VERSION"]), sample),
            run_id=env["RUN_ID"], task_id=task_id,
            policy_version=int(env["POLICY_VERSION"]), sample=sample,
            turns=turns, reward=result["reward"], passed=result["passed"],
            metrics={"n_turns": len(turns), **result["detail"]},
        )
        shutil.rmtree(work, ignore_errors=True)

        collector_url = env.get("COLLECTOR_URL")
        if collector_url:
            body = traj.to_json().encode()
            req = urllib.request.Request(
                f"{collector_url}/trajectories/{traj.key}", data=body, method="PUT",
                headers={"Content-Type": "application/json"},
            )
            try:
                urllib.request.urlopen(req, timeout=5).read()
            except Exception:  # noqa: BLE001
                pass
        sb.raw["state"] = "FINISHED"


# --------------------------------------------------------------------------- #
# Topology B
# --------------------------------------------------------------------------- #
class LocalSandbox:
    def __init__(self, sid: str) -> None:
        self.id = sid
        self.raw = {"id": sid, "state": "READY"}
        self.terminated = False

    @property
    def state(self):
        return self.raw["state"]

    @property
    def is_ready(self):
        return True

    @property
    def is_terminal(self):
        return self.terminated

    def wait_ready(self, timeout: float = 0):
        return self

    def _rewrite(self, s: str) -> str:
        return s.replace("/scratch", str(_SCRATCH)).replace("/env", str(REPO_EXAMPLE))

    def run(self, command, *, stdin=None, stdout=None, stderr=None,
            working_directory=None, timeout=None, stream=True, capture=False):
        cmd = [self._rewrite(c) for c in command]
        cwd = self._rewrite(working_directory) if working_directory else str(REPO_EXAMPLE)
        os.makedirs(cwd, exist_ok=True) if working_directory else None
        env = {**os.environ, "PYTHONPATH": str(REPO_EXAMPLE)}
        proc = subprocess.run(
            cmd, cwd=cwd, input=stdin, capture_output=True, timeout=timeout or 300, env=env,
        )
        if stdout is not None:
            stdout.write(proc.stdout)
        if stderr is not None:
            stderr.write(proc.stderr)
        return ExecResult(
            "proc_local", proc.returncode, "",
            stdout=proc.stdout if capture else b"",
            stderr=proc.stderr if capture else b"",
        )

    def terminate(self, wait: bool = False):
        self.terminated = True
        self.raw["state"] = "FINISHED"
        return self


_SCRATCH = Path(tempfile.mkdtemp(prefix="fake-scratch-"))


class _LocalSandboxes:
    def __init__(self, client):
        self._c = client

    def create(self, image, **kw):
        shutil.rmtree(_SCRATCH, ignore_errors=True)
        _SCRATCH.mkdir(parents=True, exist_ok=True)
        sb = LocalSandbox(f"sb_local_{len(self._c.created)}")
        self._c.created.append(kw)
        self._c.sandboxes_created.append(sb)
        return sb


class LocalExecClient:
    def __init__(self):
        self.created: list[dict] = []
        self.sandboxes_created: list[LocalSandbox] = []
        self.sandboxes = _LocalSandboxes(self)
