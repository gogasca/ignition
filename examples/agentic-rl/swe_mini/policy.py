"""Policy adapters — how the agent loop turns a message list into an action.

- ``OpenAICompatPolicy`` — POSTs to ``{base_url}/chat/completions`` (vLLM, or any
  OpenAI-compatible server). Requests ``logprobs`` so the trainer has per-token
  values. Standard library only.
- ``OraclePolicy`` — reads the task's known fix from ``meta.json`` and writes it,
  then submits. Deterministically reward 1.0. Used for hermetic end-to-end tests.
- ``RandomEditPolicy`` — perturbs a line and submits. Produces reward variance in
  a GRPO group without an LLM.

All return :class:`~swe_mini.trajectory.Completion`.
"""

from __future__ import annotations

import json
import random
import urllib.request
from pathlib import Path
from typing import Protocol

from .trajectory import Completion


class Policy(Protocol):
    def act(self, messages: list[dict]) -> Completion: ...


# --------------------------------------------------------------------------- #
# OpenAI-compatible (the real RL path)
# --------------------------------------------------------------------------- #
class OpenAICompatPolicy:
    def __init__(
        self,
        base_url: str,
        model: str,
        token: str = "",
        *,
        temperature: float = 0.7,
        max_tokens: int = 1024,
        want_logprobs: bool = True,
        timeout: float = 120.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.token = token
        self.temperature = temperature
        self.max_tokens = max_tokens
        self.want_logprobs = want_logprobs
        self.timeout = timeout

    def act(self, messages: list[dict]) -> Completion:
        body = {
            "model": self.model,
            "messages": messages,
            "temperature": self.temperature,
            "max_tokens": self.max_tokens,
        }
        if self.want_logprobs:
            body["logprobs"] = True
        req = urllib.request.Request(
            f"{self.base_url}/chat/completions",
            data=json.dumps(body).encode(),
            headers={
                "Content-Type": "application/json",
                **({"Authorization": f"Bearer {self.token}"} if self.token else {}),
            },
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=self.timeout) as resp:
            data = json.loads(resp.read())
        choice = data["choices"][0]
        text = choice["message"]["content"] or ""
        logprobs, tokens = _extract_logprobs(choice.get("logprobs"))
        return Completion(text=text, token_logprobs=logprobs, tokens=tokens)


def _extract_logprobs(lp: dict | None) -> tuple[list[float] | None, list[str] | None]:
    if not lp:
        return None, None
    content = lp.get("content")
    if not content:
        return None, None
    try:
        return (
            [float(tok["logprob"]) for tok in content],
            [str(tok["token"]) for tok in content],
        )
    except (KeyError, TypeError, ValueError):
        return None, None


# --------------------------------------------------------------------------- #
# Offline policies
# --------------------------------------------------------------------------- #
class OraclePolicy:
    """Solves the task from ``meta.json['solution']`` in two turns."""

    def __init__(self, task_dir: str | Path) -> None:
        meta = json.loads((Path(task_dir) / "meta.json").read_text())
        self.sol = meta["solution"]  # {"path": ..., "content": ...}
        self._turn = 0

    def act(self, messages: list[dict]) -> Completion:
        self._turn += 1
        if self._turn == 1:
            args = {"path": self.sol["path"], "content": self.sol["content"]}
            return Completion(
                text=f"ACTION: write_file\nARGS: {json.dumps(args)}",
                token_logprobs=None,
            )
        return Completion(text="ACTION: submit\nARGS: {}", token_logprobs=None)


class RandomEditPolicy:
    def __init__(self, target_path: str = "bug.py", seed: int | None = None) -> None:
        self.target = target_path
        self.rng = random.Random(seed)
        self._turn = 0

    def act(self, messages: list[dict]) -> Completion:
        self._turn += 1
        if self._turn == 1:
            return Completion(text="ACTION: read_file\nARGS: {\"path\": \"%s\"}" % self.target)
        if self._turn == 2:
            last = messages[-1]["content"]
            lines = last.splitlines() or ["x = 0"]
            i = self.rng.randrange(len(lines))
            lines[i] = lines[i] + "  # touched"
            args = {"path": self.target, "content": "\n".join(lines)}
            return Completion(text=f"ACTION: write_file\nARGS: {json.dumps(args)}")
        return Completion(text="ACTION: submit\nARGS: {}")
