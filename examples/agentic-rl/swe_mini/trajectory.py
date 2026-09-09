"""The trajectory record — the payload that flows from a rollout back to the
trainer. Deliberately plain: dataclasses + dict/JSON round-trip, standard library
only, so the in-sandbox harness and the out-of-cluster trainer share one type.

One identifier ties everything together::

    key = f"{run_id}:{task_id}:{policy_version}:{sample}"

It is the Ignition ``Idempotency-Key`` for the rollout's ``CreateSandbox``, the
collector URL suffix the harness PUTs to, and the JSONL de-dupe key.
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from typing import Any, Iterable


def make_key(run_id: str, task_id: str, policy_version: int, sample: int) -> str:
    return f"{run_id}:{task_id}:{policy_version}:{sample}"


@dataclass
class ToolCall:
    name: str
    args: dict[str, Any] = field(default_factory=dict)


@dataclass
class Completion:
    """One policy output. ``token_logprobs`` is populated only when the inference
    endpoint returns per-token logprobs (vLLM / OpenAI ``logprobs=True``); the
    offline policies leave it ``None`` and the trainer degrades to
    sequence-level advantage."""

    text: str
    tool_call: ToolCall | None = None
    token_logprobs: list[float] | None = None


@dataclass
class Turn:
    prompt: list[dict[str, Any]]  # chat messages sent to the policy this turn
    completion: Completion
    observation: str = ""  # tool result fed back to the agent


@dataclass
class Trajectory:
    key: str
    run_id: str
    task_id: str
    policy_version: int
    sample: int
    turns: list[Turn] = field(default_factory=list)
    reward: float = 0.0
    passed: bool = False
    metrics: dict[str, Any] = field(default_factory=dict)

    # -- serialization -------------------------------------------------
    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), separators=(",", ":"))

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "Trajectory":
        turns = []
        for t in d.get("turns", []):
            c = t["completion"]
            tc = c.get("tool_call")
            turns.append(
                Turn(
                    prompt=t.get("prompt", []),
                    completion=Completion(
                        text=c.get("text", ""),
                        tool_call=ToolCall(**tc) if tc else None,
                        token_logprobs=c.get("token_logprobs"),
                    ),
                    observation=t.get("observation", ""),
                )
            )
        return cls(
            key=d["key"],
            run_id=d["run_id"],
            task_id=d["task_id"],
            policy_version=int(d["policy_version"]),
            sample=int(d["sample"]),
            turns=turns,
            reward=float(d.get("reward", 0.0)),
            passed=bool(d.get("passed", False)),
            metrics=d.get("metrics", {}),
        )

    @classmethod
    def from_json(cls, s: str) -> "Trajectory":
        return cls.from_dict(json.loads(s))


def write_jsonl(path: str, trajectories: Iterable[Trajectory]) -> int:
    """Write (de-duped by ``key``, last write wins) and return the row count."""
    by_key: dict[str, Trajectory] = {}
    for t in trajectories:
        by_key[t.key] = t
    with open(path, "w", encoding="utf-8") as fh:
        for t in by_key.values():
            fh.write(t.to_json() + "\n")
    return len(by_key)


def read_jsonl(path: str) -> list[Trajectory]:
    out: list[Trajectory] = []
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line:
                out.append(Trajectory.from_json(line))
    return out
