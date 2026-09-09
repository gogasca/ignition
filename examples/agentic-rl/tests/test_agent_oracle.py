"""The agent tool-loop + OraclePolicy solve every task locally — no Ignition,
no LLM. This is the harness that runs inside the sandbox in topology A."""

from __future__ import annotations

import shutil
from pathlib import Path

from swe_mini.harness import agent, verify
from swe_mini.harness.tools import parse_action
from swe_mini.policy import OraclePolicy, RandomEditPolicy

TASKS = Path(__file__).resolve().parent.parent / "swe_mini" / "tasks"


def _fresh_work(tmp_path, tid) -> Path:
    src = TASKS / tid
    wd = tmp_path / tid
    wd.mkdir()
    for item in src.iterdir():
        if item.name == "meta.json":
            continue
        (shutil.copytree if item.is_dir() else shutil.copy2)(item, wd / item.name)
    return wd


def test_oracle_solves_all(all_task_ids, tmp_path):
    for tid in all_task_ids:
        wd = _fresh_work(tmp_path, tid)
        prompt = (TASKS / tid / "PROMPT.md").read_text()
        turns = agent.run_agent(wd, prompt, OraclePolicy(TASKS / tid), max_turns=6)
        assert turns[-1].completion.tool_call.name == "submit"
        assert verify.score(wd)["passed"] is True


def test_random_policy_produces_a_trajectory(tmp_path):
    wd = _fresh_work(tmp_path, "task_0001_sum_list")
    turns = agent.run_agent(wd, "fix it", RandomEditPolicy(seed=1), max_turns=6)
    assert turns  # a trajectory exists even though it (almost certainly) fails
    assert 0.0 <= verify.score(wd)["reward"] <= 1.0


def test_parse_action_variants():
    assert parse_action('ACTION: read_file\nARGS: {"path": "bug.py"}') == (
        "read_file", {"path": "bug.py"})
    assert parse_action("ACTION: submit\nARGS: {}") == ("submit", {})
    name, args = parse_action("no action here")
    assert name is None


def test_unknown_tool_is_an_observation_not_a_crash(work_dir):
    from swe_mini.harness import tools

    out = tools.dispatch(work_dir, "frobnicate", {})
    assert out.startswith("error: unknown tool")
