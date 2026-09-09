"""Topology-A entry point: the sandbox's main process.

    CREATING -> READY -> [this runs] -> FINISHED

Reads its assignment from the environment (Ignition injects plain values via
``environment`` and secrets via ``secretRefs``), runs one episode, verifies it,
ships the trajectory to the collector, and exits 0. A non-zero exit is reserved
for infrastructure failure — the reward lives in the trajectory, not the exit
code, so a low-reward rollout is still a successful run.

Env:
  TASK_ID            task to attempt (dir under swe_mini/tasks/)
  RUN_ID, POLICY_VERSION, SAMPLE      identify this rollout
  WORK_DIR           mutable copy of the task (default /scratch/work)
  MAX_TURNS          agent turn cap (default 12)
  INFERENCE_URL      OpenAI-compatible base URL, or "oracle://" for the offline policy
  INFERENCE_TOKEN    bearer for INFERENCE_URL              (secretRef)
  INFERENCE_MODEL    model name for the chat endpoint
  COLLECTOR_URL      where to PUT the trajectory (optional; also printed to stdout)
  COLLECTOR_TOKEN    bearer for COLLECTOR_URL              (secretRef)
"""

from __future__ import annotations

import json
import os
import shutil
import sys
import time
from pathlib import Path

from ..policy import OraclePolicy, RandomEditPolicy, OpenAICompatPolicy
from ..trajectory import Trajectory, make_key
from . import agent, collector_client, verify

TASKS_ROOT = Path(__file__).resolve().parent.parent / "tasks"


def _load_task(task_id: str, work_dir: Path) -> str:
    src = TASKS_ROOT / task_id
    if not src.is_dir():
        raise SystemExit(f"unknown TASK_ID {task_id!r}; have {[p.name for p in TASKS_ROOT.iterdir()]}")
    if work_dir.exists():
        shutil.rmtree(work_dir)
    work_dir.mkdir(parents=True)
    for item in src.iterdir():
        if item.name in ("meta.json",):
            continue
        (shutil.copytree if item.is_dir() else shutil.copy2)(item, work_dir / item.name)
    return (src / "PROMPT.md").read_text(encoding="utf-8")


def _build_policy(task_id: str):
    url = os.environ.get("INFERENCE_URL", "oracle://")
    if url.startswith("oracle://"):
        return OraclePolicy(TASKS_ROOT / task_id)
    if url.startswith("random://"):
        return RandomEditPolicy(seed=int(os.environ.get("SAMPLE", "0")))
    return OpenAICompatPolicy(
        base_url=url,
        model=os.environ.get("INFERENCE_MODEL", "policy"),
        token=os.environ.get("INFERENCE_TOKEN", ""),
        temperature=float(os.environ.get("TEMPERATURE", "0.7")),
    )


def rollout_once(
    task_id: str,
    work_dir: str | Path,
    policy,
    *,
    run_id: str = "adhoc",
    policy_version: int = 0,
    sample: int = 0,
    max_turns: int = 12,
) -> Trajectory:
    """Run one episode in ``work_dir`` and return its trajectory. Shared by the
    in-sandbox entry point (topology A) and the offline driver."""
    work_dir = Path(work_dir)
    started = time.time()
    prompt = _load_task(task_id, work_dir)
    turns = agent.run_agent(work_dir, prompt, policy, max_turns=max_turns)
    result = verify.score(work_dir)
    return Trajectory(
        key=make_key(run_id, task_id, policy_version, sample),
        run_id=run_id,
        task_id=task_id,
        policy_version=policy_version,
        sample=sample,
        turns=turns,
        reward=result["reward"],
        passed=result["passed"],
        metrics={
            "n_turns": len(turns),
            "wall_seconds": round(time.time() - started, 3),
            **result["detail"],
        },
    )


def main() -> int:
    task_id = os.environ["TASK_ID"]
    work_dir = Path(os.environ.get("WORK_DIR", "/scratch/work"))
    traj = rollout_once(
        task_id,
        work_dir,
        _build_policy(task_id),
        run_id=os.environ.get("RUN_ID", "adhoc"),
        policy_version=int(os.environ.get("POLICY_VERSION", "0")),
        sample=int(os.environ.get("SAMPLE", "0")),
        max_turns=int(os.environ.get("MAX_TURNS", "12")),
    )

    # Always emit the sentinel line (topology-B parsing / debugging).
    print(verify.RESULT_SENTINEL + " " + json.dumps(
        {"key": traj.key, "reward": traj.reward, "passed": traj.passed, "metrics": traj.metrics}
    ))

    collector_url = os.environ.get("COLLECTOR_URL", "")
    if collector_url:
        ok = collector_client.push(collector_url, os.environ.get("COLLECTOR_TOKEN", ""), traj)
        if not ok:
            print("WARNING: trajectory not delivered to collector", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
