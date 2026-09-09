"""The agent tool-loop. Model-agnostic: it feeds a message list to a
:class:`~swe_mini.policy.Policy`, parses an ``ACTION`` out of the completion,
runs the tool, appends the observation, and repeats until ``submit`` or the turn
cap. Returns the list of :class:`~swe_mini.trajectory.Turn` for the trajectory.
"""

from __future__ import annotations

from pathlib import Path

from ..policy import Policy
from ..trajectory import Completion, ToolCall, Turn
from . import tools

SYSTEM_PROMPT = """\
You are a coding agent fixing a bug in a small Python module. Each turn, reply \
with exactly one action in this format:

ACTION: <one of: list_files, read_file, write_file, run_tests, submit>
ARGS: <a single-line JSON object>

Examples:
ACTION: read_file
ARGS: {"path": "bug.py"}

ACTION: write_file
ARGS: {"path": "bug.py", "content": "def solve():\\n    return 42\\n"}

ACTION: run_tests
ARGS: {}

When the tests pass, or you are confident, reply:
ACTION: submit
ARGS: {}

Only the file(s) under the work directory exist. Keep edits minimal.
"""


def run_agent(
    work_dir: str | Path,
    task_prompt: str,
    policy: Policy,
    *,
    max_turns: int = 12,
) -> list[Turn]:
    messages: list[dict] = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": task_prompt},
    ]
    turns: list[Turn] = []

    for _ in range(max_turns):
        prompt_snapshot = [dict(m) for m in messages]
        completion: Completion = policy.act(messages)
        name, args = tools.parse_action(completion.text)
        messages.append({"role": "assistant", "content": completion.text})

        if name is None:
            observation = (
                "error: no ACTION found. Reply with 'ACTION: <tool>' and "
                "'ARGS: <json>'."
            )
        else:
            observation = tools.dispatch(work_dir, name, args)

        completion.tool_call = ToolCall(name=name or "none", args=args)
        turns.append(
            Turn(prompt=prompt_snapshot, completion=completion, observation=observation)
        )

        if name == "submit":
            break
        messages.append({"role": "user", "content": f"OBSERVATION:\n{observation}"})

    return turns
