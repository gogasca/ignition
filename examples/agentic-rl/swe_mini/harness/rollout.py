"""Topology-A entry point: the sandbox's main process.

    CREATING -> READY -> [this runs] -> FINISHED

Reads its assignment from CLI flags (Ignition's ``CreateSandbox`` has no plain
``environment`` field — only ``secretRefs``, one Secret Manager secret per named
env var — so the controller passes non-secret config as ``args``, and this is
the ``command`` the sandbox's main process runs; see
``driver/controller.py::_rollout_one``), runs one episode, verifies it, ships
the trajectory to the collector, and exits 0. A non-zero exit is reserved for
infrastructure failure — the reward lives in the trajectory, not the exit code,
so a low-reward rollout is still a successful run.

Flags (each also falls back to an env var of the same name, for local /
``docker run -e`` debugging outside Ignition):
  --task-id           task to attempt (dir under swe_mini/tasks/)      TASK_ID
  --run-id, --policy-version, --sample    identify this rollout
  --work-dir          mutable copy of the task (default /scratch/work) WORK_DIR
  --max-turns         agent turn cap (default 12)                     MAX_TURNS
  --inference-url     OpenAI-compatible base URL, or "oracle://"      INFERENCE_URL
  --inference-model   model name for the chat endpoint                INFERENCE_MODEL
  --collector-url     where to PUT the trajectory (optional)          COLLECTOR_URL

INFERENCE_TOKEN / COLLECTOR_TOKEN are read from the environment only — those
come from real, working Ignition ``secretRefs`` (Secret Manager -> Pod env), so
they never belong in argv.
"""

from __future__ import annotations

import argparse
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


def _build_policy(args: argparse.Namespace):
    if args.inference_url.startswith("oracle://"):
        return OraclePolicy(TASKS_ROOT / args.task_id)
    if args.inference_url.startswith("random://"):
        return RandomEditPolicy(seed=args.sample)
    return OpenAICompatPolicy(
        base_url=args.inference_url,
        model=args.inference_model,
        token=os.environ.get("INFERENCE_TOKEN", ""),  # secretRefs -> Pod env; real
        temperature=float(os.environ.get("TEMPERATURE", "0.7")),
    )


def _parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Run one episode inside the sandbox.")
    p.add_argument("--task-id", default=os.environ.get("TASK_ID"))
    p.add_argument("--run-id", default=os.environ.get("RUN_ID", "adhoc"))
    p.add_argument("--policy-version", type=int, default=int(os.environ.get("POLICY_VERSION", "0")))
    p.add_argument("--sample", type=int, default=int(os.environ.get("SAMPLE", "0")))
    p.add_argument("--max-turns", type=int, default=int(os.environ.get("MAX_TURNS", "12")))
    p.add_argument("--work-dir", default=os.environ.get("WORK_DIR", "/scratch/work"))
    p.add_argument("--inference-url", default=os.environ.get("INFERENCE_URL", "oracle://"))
    p.add_argument("--inference-model", default=os.environ.get("INFERENCE_MODEL", "policy"))
    p.add_argument("--collector-url", default=os.environ.get("COLLECTOR_URL", ""))
    args = p.parse_args(argv)
    if not args.task_id:
        p.error("--task-id is required (or set TASK_ID)")
    return args


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


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(sys.argv[1:] if argv is None else argv)
    work_dir = Path(args.work_dir)
    traj = rollout_once(
        args.task_id,
        work_dir,
        _build_policy(args),
        run_id=args.run_id,
        policy_version=args.policy_version,
        sample=args.sample,
        max_turns=args.max_turns,
    )

    # Always emit the sentinel line (topology-B parsing / debugging).
    print(verify.RESULT_SENTINEL + " " + json.dumps(
        {"key": traj.key, "reward": traj.reward, "passed": traj.passed, "metrics": traj.metrics}
    ))

    if args.collector_url:
        ok = collector_client.push(args.collector_url, os.environ.get("COLLECTOR_TOKEN", ""), traj)
        if not ok:
            print("WARNING: trajectory not delivered to collector", file=sys.stderr)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
