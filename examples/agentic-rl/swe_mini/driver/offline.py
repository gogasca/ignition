"""Run the rollout harness locally, with no Ignition at all — the same
``swe_mini.harness`` code that runs inside the sandbox, driven here in temp
directories. Used by ``make smoke-offline`` and as a fast inner loop while
iterating on tasks or the agent.

    python -m swe_mini.driver.offline --run-id offline --group-size 4
"""

from __future__ import annotations

import argparse
import tempfile

from ..harness.rollout import TASKS_ROOT, rollout_once
from ..policy import OraclePolicy, RandomEditPolicy
from ..trajectory import write_jsonl
from .config import RunConfig


def _policy(task_id: str, sample: int, kind: str):
    if kind == "oracle":
        return OraclePolicy(TASKS_ROOT / task_id)
    if kind == "random":
        return RandomEditPolicy(seed=sample)
    # "mixed": oracle on even samples, random on odd — gives a GRPO group spread
    return OraclePolicy(TASKS_ROOT / task_id) if sample % 2 == 0 else RandomEditPolicy(seed=sample)


def run(cfg: RunConfig, policy_kind: str = "mixed") -> str:
    import os

    results = []
    for task_id in cfg.tasks:
        for sample in range(cfg.group_size):
            with tempfile.TemporaryDirectory() as wd:
                results.append(rollout_once(
                    task_id, wd, _policy(task_id, sample, policy_kind),
                    run_id=cfg.run_id, policy_version=cfg.policy_version,
                    sample=sample, max_turns=cfg.max_turns,
                ))
    os.makedirs(cfg.out_dir, exist_ok=True)
    path = os.path.join(cfg.out_dir, f"{cfg.run_id}.jsonl")
    write_jsonl(path, results)
    solved = sum(1 for t in results if t.passed)
    print(f"offline run {cfg.run_id}: {len(results)} trajectories, {solved} solved -> {path}")
    return path


def main(argv: list[str]) -> int:
    cfg = RunConfig.from_env()
    p = argparse.ArgumentParser(description="Run swe-mini rollouts locally (no Ignition).")
    cfg.add_cli(p)
    p.add_argument("--policy", choices=["oracle", "random", "mixed"], default="mixed")
    a = p.parse_args(argv[1:])
    cfg.apply_cli(a)
    run(cfg, a.policy)
    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(main(sys.argv))
