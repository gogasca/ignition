"""Load a run's trajectories and group them for GRPO.

A "group" is the set of rollouts that share a prompt — here, all samples of one
task at one policy version. Advantage is computed within a group, so the group
must have ≥ 2 members with some reward spread to produce a non-zero signal.
"""

from __future__ import annotations

from collections import defaultdict

from ..trajectory import Trajectory, read_jsonl


def load(path: str) -> list[Trajectory]:
    return read_jsonl(path)


def group_by_prompt(trajectories: list[Trajectory]) -> dict[tuple[str, int], list[Trajectory]]:
    groups: dict[tuple[str, int], list[Trajectory]] = defaultdict(list)
    for t in trajectories:
        groups[(t.task_id, t.policy_version)].append(t)
    return dict(groups)


def summary(trajectories: list[Trajectory]) -> dict:
    n = len(trajectories)
    if n == 0:
        return {"n": 0}
    return {
        "n": n,
        "solved": sum(1 for t in trajectories if t.passed),
        "pass_rate": round(sum(1 for t in trajectories if t.passed) / n, 4),
        "mean_reward": round(sum(t.reward for t in trajectories) / n, 4),
        "mean_turns": round(sum(t.metrics.get("n_turns", len(t.turns)) for t in trajectories) / n, 2),
        "errors": sum(1 for t in trajectories if t.metrics.get("error")),
    }
