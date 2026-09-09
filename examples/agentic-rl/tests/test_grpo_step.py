"""GRPO advantage math and the training-batch shape."""

from __future__ import annotations

import json

from swe_mini.trainer import grpo_step, replay_buffer
from swe_mini.trajectory import Completion, Trajectory, Turn


def _traj(task, sample, reward, *, pv=0, logprobs=None):
    return Trajectory(
        key=f"r:{task}:{pv}:{sample}", run_id="r", task_id=task, policy_version=pv,
        sample=sample, reward=reward, passed=reward == 1.0,
        turns=[Turn(prompt=[{"role": "user", "content": "x"}],
                    completion=Completion(text="ACTION: submit\nARGS: {}",
                                          token_logprobs=logprobs))],
    )


def test_advantage_is_group_relative():
    group = [_traj("t", 0, 0.0), _traj("t", 1, 1.0), _traj("t", 2, 0.5)]
    adv = grpo_step.advantages(group)
    assert abs(sum(adv.values())) < 1e-6           # zero-mean within the group
    assert adv["r:t:0:0"] < adv["r:t:0:2"] < adv["r:t:0:1"]


def test_identical_rewards_give_zero_advantage():
    group = [_traj("t", i, 0.4) for i in range(4)]
    assert all(abs(a) < 1e-3 for a in grpo_step.advantages(group).values())


def test_build_batch_groups_by_task_and_version():
    trajs = [_traj("a", 0, 0.0), _traj("a", 1, 1.0), _traj("b", 0, 0.2), _traj("b", 1, 0.2)]
    rows = grpo_step.build_batch(trajs)
    assert len(rows) == 4
    a_rows = [r for r in rows if r.task_id == "a"]
    assert {round(r.advantage, 3) for r in a_rows} == {-1.0, 1.0}


def test_run_writes_batch_and_summary(tmp_path):
    trajs = [_traj("a", 0, 0.0, logprobs=[-0.1, -0.2]),
             _traj("a", 1, 1.0, logprobs=[-0.3, -0.4]),
             _traj("b", 0, 0.5), _traj("b", 1, 0.5)]
    run_path = tmp_path / "r.jsonl"
    from swe_mini.trajectory import write_jsonl
    write_jsonl(str(run_path), trajs)

    report = grpo_step.run(str(run_path), out_dir=str(tmp_path / "batch"))
    assert report["rows"] == 4
    assert report["groups"] == 2
    assert report["nonzero_advantage_groups"] == 1   # only group "a" has spread
    assert report["has_logprobs"] is True
    assert report["stub_loss"] is not None

    batch = [json.loads(ln) for ln in (tmp_path / "batch" / "r.jsonl").read_text().splitlines()]
    assert set(batch[0]) == {"task_id", "policy_version", "prompt", "completion",
                             "advantage", "token_logprobs"}


def test_summary_counts_errors():
    trajs = [_traj("a", 0, 0.0), _traj("a", 1, 1.0)]
    trajs[0].metrics["error"] = "startup timeout"
    s = replay_buffer.summary(trajs)
    assert s["errors"] == 1
    assert s["solved"] == 1
