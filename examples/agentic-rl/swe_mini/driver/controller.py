"""Topology A — the rollout controller.

For each (task, sample) in the run it creates one CPU sandbox from the admitted
env image, waits for it to finish, collects the trajectory the in-sandbox harness
pushed, and always tears the sandbox down. Concurrency is bounded; admission
races / transient API errors get bounded exponential backoff with one retry on a
fresh sandbox.

The trainer is *not* in this loop — the controller just produces
``{out_dir}/{run_id}.jsonl``.
"""

from __future__ import annotations

import json
import random
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed

from ..trajectory import Trajectory, make_key
from .config import RunConfig
from .collector import Collector

try:  # the SDK is a real dependency, but keep import errors legible
    from ignition_sandbox import Client
    from ignition_sandbox.errors import APIError, ConflictError, IgnitionError, TimeoutError
except ImportError as e:  # pragma: no cover
    raise SystemExit(
        "ignition-sandbox SDK not importable — `pip install -e '.[dev]'` in "
        f"examples/agentic-rl installs it as a path dependency ({e})"
    )

_RETRYABLE = (TimeoutError, APIError, IgnitionError)


class _RemoteCollector:
    """Poll an externally-run collector over HTTP (used when --collector-url is set)."""

    def __init__(self, url: str, token: str = "") -> None:
        self.url = url.rstrip("/")
        self.token = token

    def wait_for(self, key: str, timeout: float = 60.0, poll: float = 1.0):
        headers = {"Authorization": f"Bearer {self.token}"} if self.token else {}
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            req = urllib.request.Request(f"{self.url}/trajectories/{key}", headers=headers)
            try:
                with urllib.request.urlopen(req, timeout=10) as resp:
                    if resp.status == 200:
                        return Trajectory.from_dict(json.loads(resp.read()))
            except urllib.error.HTTPError as e:
                if e.code != 404:
                    raise
            except (urllib.error.URLError, TimeoutError):
                pass
            time.sleep(poll)
        return None


def _rollout_one(client: Client, cfg: RunConfig, collector, task_id: str, sample: int) -> Trajectory:
    key = make_key(cfg.run_id, task_id, cfg.policy_version, sample)
    env = cfg.sandbox_env(task_id, sample, collector.url)
    last_err = ""

    for attempt in range(2):
        sb = None
        try:
            sb = client.sandboxes.create(
                cfg.env_image,
                accelerator="NONE",
                cpu_milli=cfg.cpu_milli,
                memory_mib=cfg.memory_mib,
                internet=True,  # topology A: harness dials inference + collector
                env=env,
                secret_refs=cfg.secret_refs() or None,
                labels={"run": cfg.run_id, "task": task_id, "pv": str(cfg.policy_version)},
                maximum_runtime_seconds=cfg.episode_seconds,
                idle_seconds=0,
                startup_seconds=cfg.startup_seconds,
                idempotency_key=key,
                wait=True,
                wait_timeout=cfg.startup_seconds + 30,
            )
            for snap in sb.watch():
                if snap.is_terminal:
                    break

            procs = sb.processes.list()
            exit_code = procs[0].refresh().exit_code if procs else None

            traj = collector.wait_for(key, timeout=cfg.collector_wait_seconds)
            if traj is not None:
                return traj
            last_err = f"no trajectory (sandbox {sb.state}, exit={exit_code})"

        except ConflictError:
            # Idempotency replay — a rollout for this key already ran. Its
            # trajectory is (or will be) in the collector.
            traj = collector.wait_for(key, timeout=cfg.collector_wait_seconds)
            if traj is not None:
                return traj
            last_err = "conflict, and no trajectory recorded"
            break
        except _RETRYABLE as e:  # noqa: PERF203
            last_err = f"{type(e).__name__}: {e}"
        finally:
            if sb is not None:
                try:
                    sb.terminate(wait=False)
                except _RETRYABLE:
                    pass

        time.sleep(min(2 ** attempt, 8) + random.random())

    return Trajectory(
        key=key, run_id=cfg.run_id, task_id=task_id,
        policy_version=cfg.policy_version, sample=sample,
        reward=0.0, passed=False, metrics={"error": last_err or "rollout failed"},
    )


def run(cfg: RunConfig, client: Client | None = None, collector=None) -> str:
    client = client or Client()
    owns_collector = collector is None and not cfg.collector_url

    if cfg.collector_url:
        collector = collector or _RemoteCollector(cfg.collector_url, cfg.collector_token)
        cm = None
    elif owns_collector:
        cm = Collector(out_dir=cfg.out_dir, token=cfg.collector_token)
        collector = cm.__enter__()
    else:
        cm = None

    started = time.time()
    results: list[Trajectory] = []
    try:
        jobs = [(t, s) for t in cfg.tasks for s in range(cfg.group_size)]
        with ThreadPoolExecutor(max_workers=cfg.max_inflight) as pool:
            futs = {
                pool.submit(_rollout_one, client, cfg, collector, t, s): (t, s)
                for t, s in jobs
            }
            for fut in as_completed(futs):
                traj = fut.result()
                results.append(traj)
                mark = "ok " if traj.passed else "   "
                print(f"  [{mark}] {traj.key}  reward={traj.reward:.3f}  {traj.metrics.get('error','')}")
    finally:
        if cm is not None:
            cm.__exit__(None, None, None)

    from ..trajectory import write_jsonl
    import os

    os.makedirs(cfg.out_dir, exist_ok=True)
    path = os.path.join(cfg.out_dir, f"{cfg.run_id}.jsonl")
    n = write_jsonl(path, results)
    solved = sum(1 for t in results if t.passed)
    mean_r = sum(t.reward for t in results) / max(len(results), 1)
    print(
        f"run {cfg.run_id}: {n} trajectories, {solved} solved, mean reward {mean_r:.3f}, "
        f"{time.time() - started:.1f}s -> {path}"
    )
    return path


def main(argv: list[str]) -> int:
    import argparse

    cfg = RunConfig.from_env()
    p = argparse.ArgumentParser(description="Run a batch of topology-A rollouts on Ignition.")
    cfg.add_cli(p)
    cfg.apply_cli(p.parse_args(argv[1:]))
    run(cfg)
    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(main(sys.argv))
