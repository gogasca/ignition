"""Topology-A controller against FakeIgnition + a real in-process collector.

Exercises: fan-out over (task, sample), trajectory round-trip through the
collector HTTP path, idempotency-replay (ConflictError) handling, always-terminate,
and the consolidated JSONL output.
"""

from __future__ import annotations

from swe_mini.driver import controller
from swe_mini.driver.collector import Collector
from swe_mini.driver.config import RunConfig
from swe_mini.policy import OraclePolicy
from swe_mini.trajectory import make_key, read_jsonl

from .fake_ignition import FakeClient


def _cfg(tmp_path, **kw) -> RunConfig:
    c = RunConfig(
        run_id="t", policy_version=0,
        tasks=["task_0001_sum_list", "task_0003_fib"],
        group_size=3, max_inflight=4,
        collector_wait_seconds=5,
        out_dir=str(tmp_path / "traj"),
    )
    for k, v in kw.items():
        setattr(c, k, v)
    return c


def test_full_batch_round_trips(tmp_path):
    cfg = _cfg(tmp_path)
    client = FakeClient(policy_factory=lambda td, s: OraclePolicy(td))  # all solve
    with Collector(out_dir=cfg.out_dir) as col:
        path = controller.run(cfg, client=client, collector=col)

    rows = read_jsonl(path)
    assert len(rows) == cfg.group_size * len(cfg.tasks)
    assert all(t.passed for t in rows)
    assert all(t.reward == 1.0 for t in rows)
    # every sandbox that was created got terminated
    assert client.sandboxes_created
    assert all(sb.terminated for sb in client.sandboxes_created)


def test_reward_variance_survives(tmp_path):
    cfg = _cfg(tmp_path)
    client = FakeClient()  # default: even samples oracle, odd samples random
    with Collector(out_dir=cfg.out_dir) as col:
        path = controller.run(cfg, client=client, collector=col)
    rewards = {t.key: t.reward for t in read_jsonl(path)}
    assert min(rewards.values()) < max(rewards.values())  # a usable GRPO group


def test_idempotency_replay_is_handled(tmp_path):
    cfg = _cfg(tmp_path, tasks=["task_0001_sum_list"], group_size=1)
    key = make_key("t", "task_0001_sum_list", 0, 0)

    client = FakeClient()
    client.conflict_keys.add(key)  # create() will raise ConflictError for this key

    with Collector(out_dir=cfg.out_dir) as col:
        # Pre-seed the trajectory the "already-running" rollout would have pushed.
        import urllib.request
        from swe_mini.trajectory import Trajectory
        seed = Trajectory(key=key, run_id="t", task_id="task_0001_sum_list",
                          policy_version=0, sample=0, reward=0.8, passed=False)
        urllib.request.urlopen(urllib.request.Request(
            f"{col.url}/trajectories/{key}", data=seed.to_json().encode(), method="PUT"))

        path = controller.run(cfg, client=client, collector=col)

    rows = read_jsonl(path)
    assert len(rows) == 1
    assert rows[0].reward == 0.8  # picked up the pre-existing trajectory


def test_missing_trajectory_becomes_a_failure_record(tmp_path):
    cfg = _cfg(tmp_path, tasks=["task_0001_sum_list"], group_size=1)

    class SilentClient(FakeClient):
        def _play_harness(self, sb, kw):  # never pushes a trajectory
            import time
            time.sleep(0.05)
            sb.raw["state"] = "FINISHED"

    with Collector(out_dir=cfg.out_dir) as col:
        path = controller.run(cfg, client=SilentClient(), collector=col)
    rows = read_jsonl(path)
    assert len(rows) == 1
    assert rows[0].reward == 0.0
    assert "error" in rows[0].metrics
