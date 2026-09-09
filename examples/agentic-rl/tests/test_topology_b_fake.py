"""Topology-B loop against LocalExecClient: the driver-side agent loop, with
tool calls executed as real subprocesses (cp / cat / tee / pytest / verify) with
Ignition paths rewritten. No Ignition, no LLM."""

from __future__ import annotations

from swe_mini.driver import topology_b
from swe_mini.driver.config import RunConfig
from swe_mini.policy import OraclePolicy, RandomEditPolicy

from .fake_ignition import LocalExecClient, TASKS


def _cfg(**kw) -> RunConfig:
    c = RunConfig(run_id="tb", policy_version=0, group_size=1, max_turns=8,
                  episode_seconds=120, startup_seconds=1)
    for k, v in kw.items():
        setattr(c, k, v)
    return c


def test_oracle_solves_via_exec_stream():
    cfg = _cfg()
    client = LocalExecClient()
    traj = topology_b.run_one(client, cfg, OraclePolicy(TASKS / "task_0001_sum_list"),
                              "task_0001_sum_list")
    assert traj.passed is True
    assert traj.reward == 1.0
    assert traj.turns[-1].completion.tool_call.name == "submit"
    assert client.sandboxes_created[0].terminated


def test_random_policy_still_yields_a_scored_trajectory():
    cfg = _cfg()
    traj = topology_b.run_one(LocalExecClient(), cfg, RandomEditPolicy(seed=3),
                              "task_0004_flatten")
    assert 0.0 <= traj.reward <= 1.0
    assert traj.metrics["n_turns"] >= 1


def test_result_sentinel_is_parsed_from_stdout():
    out = "noise\n---IGN-RESULT--- {\"reward\": 0.5, \"passed\": false, \"detail\": {}}\nmore"
    assert topology_b._parse_result(out) == {"reward": 0.5, "passed": False, "detail": {}}
