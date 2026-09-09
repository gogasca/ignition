"""Topology B — the driver drives a bare sandbox over the exec stream.

The sandbox just idles (``sleep``); the agent loop runs here, one ``sb.run`` per
tool call, reading observations back off the exec stdout stream. Nothing leaves
the sandbox except through the control plane, so it needs no egress — good for
bring-up, debugging a single task, and fully-hermetic tests.

Slower than topology A (every turn is a gateway round trip) and the in-memory
exec replay buffer has no offset reconnect, so this is not the shape for a big
training run — see docs/guides/agentic-rl-example.md.
"""

from __future__ import annotations

import io
import time

from ..harness import tools, verify
from ..policy import Policy
from ..trajectory import Completion, ToolCall, Trajectory, Turn, make_key
from .config import RunConfig

WORK = "/scratch/work"
TASKS_IN_IMAGE = "/env/swe_mini/tasks"


def _run(sb, cmd: list[str], *, stdin: bytes | None = None, cwd: str | None = None) -> tuple[int | None, str]:
    """sb.run(...) returning (exit_code, combined stdout+stderr text)."""
    try:
        res = sb.run(cmd, stdin=stdin, working_directory=cwd, capture=True)
        return res.exit_code, (res.stdout + res.stderr).decode("utf-8", "replace")
    except TypeError:
        # SDK without capture=: tee into our own buffers.
        buf = io.BytesIO()
        res = sb.run(cmd, stdin=stdin, working_directory=cwd, stdout=buf, stderr=buf)
        return res.exit_code, buf.getvalue().decode("utf-8", "replace")


def _tool_in_sandbox(sb, name: str, args: dict) -> str:
    if name == "list_files":
        _, out = _run(sb, ["find", ".", "-type", "f", "-not", "-path", "*/__pycache__/*"], cwd=WORK)
        return "files:\n" + out.strip()
    if name == "read_file":
        code, out = _run(sb, ["cat", args["path"]], cwd=WORK)
        return out if code == 0 else f"error: cannot read {args['path']}"
    if name == "write_file":
        code, out = _run(sb, ["tee", args["path"]], stdin=args["content"].encode(), cwd=WORK)
        return f"wrote {len(args['content'])} bytes to {args['path']}" if code == 0 else f"error: {out}"
    if name == "run_tests":
        _, out = _run(sb, ["python", "-m", "pytest", "-q", "-p", "no:cacheprovider", "--tb=short", WORK])
        return out[-tools.MAX_OBS_CHARS:]
    if name == "submit":
        return "submitted"
    return f"error: unknown tool {name!r}"


def run_one(client, cfg: RunConfig, policy: Policy, task_id: str, sample: int = 0) -> Trajectory:
    from ..harness.agent import SYSTEM_PROMPT

    key = make_key(cfg.run_id, task_id, cfg.policy_version, sample)
    started = time.time()
    sb = client.sandboxes.create(
        cfg.env_image,
        command=["sleep", str(cfg.episode_seconds)],
        accelerator="NONE",
        cpu_milli=cfg.cpu_milli,
        memory_mib=cfg.memory_mib,
        internet=False,
        labels={"run": cfg.run_id, "task": task_id, "topology": "b"},
        maximum_runtime_seconds=cfg.episode_seconds,
        idle_seconds=0,
        startup_seconds=cfg.startup_seconds,
        idempotency_key=key,
        wait=True,
        wait_timeout=cfg.startup_seconds + 30,
    )
    turns: list[Turn] = []
    try:
        _run(sb, ["sh", "-c",
                  f"rm -rf {WORK} && mkdir -p {WORK} && "
                  f"cp -r {TASKS_IN_IMAGE}/{task_id}/. {WORK}/ && rm -f {WORK}/meta.json"])

        prompt = _run(sb, ["cat", "PROMPT.md"], cwd=WORK)[1]
        messages = [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": prompt},
        ]
        for _ in range(cfg.max_turns):
            snapshot = [dict(m) for m in messages]
            completion: Completion = policy.act(messages)
            name, args = tools.parse_action(completion.text)
            messages.append({"role": "assistant", "content": completion.text})
            observation = (
                "error: no ACTION found."
                if name is None
                else _tool_in_sandbox(sb, name, args)
            )
            completion.tool_call = ToolCall(name=name or "none", args=args)
            turns.append(Turn(prompt=snapshot, completion=completion, observation=observation))
            if name == "submit":
                break
            messages.append({"role": "user", "content": f"OBSERVATION:\n{observation}"})

        code, out = _run(sb, ["python", "-m", "swe_mini.harness.verify", WORK])
        result = _parse_result(out)
    finally:
        sb.terminate(wait=False)

    return Trajectory(
        key=key, run_id=cfg.run_id, task_id=task_id,
        policy_version=cfg.policy_version, sample=sample, turns=turns,
        reward=result["reward"], passed=result["passed"],
        metrics={"n_turns": len(turns), "wall_seconds": round(time.time() - started, 3),
                 **result.get("detail", {})},
    )


def _parse_result(stdout: str) -> dict:
    import json

    for line in stdout.splitlines():
        if line.startswith(verify.RESULT_SENTINEL):
            return json.loads(line[len(verify.RESULT_SENTINEL):].strip())
    return {"reward": 0.0, "passed": False, "detail": {"error": "no result sentinel"}}


def main(argv: list[str]) -> int:
    import argparse

    from ignition_sandbox import Client
    from ..harness.rollout import TASKS_ROOT
    from ..policy import OraclePolicy, OpenAICompatPolicy

    cfg = RunConfig.from_env()
    p = argparse.ArgumentParser(description="Run one topology-B rollout on Ignition.")
    cfg.add_cli(p)
    p.add_argument("--task", default=cfg.tasks[0])
    a = p.parse_args(argv[1:])
    cfg.apply_cli(a)

    if cfg.inference_url.startswith("oracle://"):
        policy = OraclePolicy(TASKS_ROOT / a.task)
    else:
        policy = OpenAICompatPolicy(cfg.inference_url, cfg.inference_model, cfg.inference_token)

    traj = run_one(Client(), cfg, policy, a.task)
    print(f"{traj.key}  reward={traj.reward:.3f}  passed={traj.passed}  turns={len(traj.turns)}")
    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(main(sys.argv))
