"""Turn a run's trajectories into a GRPO training batch.

Per group g with rewards r_i:

    A_i = (r_i - mean(r_g)) / (std(r_g) + eps)

Every turn of trajectory i inherits that trajectory's advantage (outcome-
supervised: the whole rollout is credited/blamed for its verified reward). Each
emitted row is one policy-gradient example::

    {"task_id", "policy_version", "prompt": [chat messages],
     "completion": "<assistant text>", "advantage": float,
     "token_logprobs": [float] | null}

`trl.GRPOTrainer` / the `verifiers` library consume exactly this shape. The stub
"loss" printed at the end is the REINFORCE objective you would minimize
(-mean(A * sum logp)) — reported for monitoring, not backpropagated.
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
from dataclasses import dataclass

from ..trajectory import Trajectory
from . import replay_buffer

EPS = 1e-6


@dataclass
class BatchRow:
    task_id: str
    policy_version: int
    prompt: list
    completion: str
    advantage: float
    token_logprobs: list | None


def advantages(group: list[Trajectory]) -> dict[str, float]:
    rewards = [t.reward for t in group]
    mean = statistics.fmean(rewards)
    std = statistics.pstdev(rewards) if len(rewards) > 1 else 0.0
    return {t.key: (t.reward - mean) / (std + EPS) for t in group}


def build_batch(trajectories: list[Trajectory]) -> list[BatchRow]:
    rows: list[BatchRow] = []
    for (_task, _pv), group in replay_buffer.group_by_prompt(trajectories).items():
        adv = advantages(group)
        for t in group:
            a = adv[t.key]
            for turn in t.turns:
                rows.append(BatchRow(
                    task_id=t.task_id,
                    policy_version=t.policy_version,
                    prompt=turn.prompt,
                    completion=turn.completion.text,
                    advantage=round(a, 6),
                    token_logprobs=turn.completion.token_logprobs,
                ))
    return rows


def stub_loss(rows: list[BatchRow]) -> float | None:
    """-mean(advantage * sum(token_logprobs)) over rows that carry logprobs."""
    terms = [
        r.advantage * sum(r.token_logprobs)
        for r in rows
        if r.token_logprobs
    ]
    if not terms:
        return None
    return round(-statistics.fmean(terms), 6)


def run(run_path: str, out_dir: str = "training_batch") -> dict:
    trajectories = replay_buffer.load(run_path)
    run_id = trajectories[0].run_id if trajectories else os.path.splitext(os.path.basename(run_path))[0]
    rows = build_batch(trajectories)

    os.makedirs(out_dir, exist_ok=True)
    batch_path = os.path.join(out_dir, f"{run_id}.jsonl")
    with open(batch_path, "w", encoding="utf-8") as fh:
        for r in rows:
            fh.write(json.dumps({
                "task_id": r.task_id,
                "policy_version": r.policy_version,
                "prompt": r.prompt,
                "completion": r.completion,
                "advantage": r.advantage,
                "token_logprobs": r.token_logprobs,
            }, separators=(",", ":")) + "\n")

    groups = replay_buffer.group_by_prompt(trajectories)
    report = {
        "run_id": run_id,
        "batch_path": batch_path,
        "rows": len(rows),
        "groups": len(groups),
        "nonzero_advantage_groups": sum(
            1 for g in groups.values() if statistics.pstdev([t.reward for t in g] or [0]) > 0
        ),
        "has_logprobs": any(r.token_logprobs for r in rows),
        "stub_loss": stub_loss(rows),
        **replay_buffer.summary(trajectories),
    }
    with open(os.path.join(out_dir, f"{run_id}.summary.json"), "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2)
    return report


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description="Build a GRPO training batch from a run's trajectories.")
    p.add_argument("--run", required=True, help="path to trajectories/<run_id>.jsonl (or a run_id)")
    p.add_argument("--trajectories-dir", default="trajectories")
    p.add_argument("--out-dir", default="training_batch")
    a = p.parse_args(argv[1:])
    run_path = a.run if os.path.exists(a.run) else os.path.join(a.trajectories_dir, f"{a.run}.jsonl")
    report = run(run_path, a.out_dir)
    print(json.dumps(report, indent=2))
    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(main(sys.argv))
