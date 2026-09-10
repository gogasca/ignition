"""The outer RL loop, stitched together.

    for step in range(steps):
        controller.run(...)        # produce trajectories on Ignition sandboxes
        grpo_step.run(...)         # -> GRPO training batch + summary
        <optimizer step>           # NOT IMPLEMENTED — the documented next step
        reload_inference(...)      # push new weights to vLLM (no-op hook here)
        policy_version += 1

Ignition appears only in ``controller.run``. Everything else is ordinary
out-of-cluster training code. Run with ``INFERENCE_URL=oracle://`` for a
hermetic dry run of the plumbing, or point it at a real vLLM for a real (if
un-optimized) loop.
"""

from __future__ import annotations

import argparse
import json
import sys

from .driver import controller
from .driver.config import RunConfig
from .trainer import grpo_step


def reload_inference(policy_version: int) -> None:
    """Seam for weight sync. A real implementation swaps LoRA / full weights on
    the vLLM server(s) and waits for them to be live before the next batch."""
    print(f"[reload_inference] would publish policy_version={policy_version} to the inference fleet")


def main(argv: list[str]) -> int:
    cfg = RunConfig.from_env()
    p = argparse.ArgumentParser(description="Run the swe-mini RL outer loop.")
    cfg.add_cli(p)
    p.add_argument("--steps", type=int, default=3)
    p.add_argument("--topology", choices=["a", "b", "offline"], default="a")
    p.add_argument("--backend", choices=["stub", "trl"], default="stub",
                   help="stub: GRPO batch + reported loss, no optimizer. "
                        "trl: real trl.GRPOTrainer step (needs [train] + a GPU + vLLM).")
    p.add_argument("--model", default="Qwen/Qwen3-0.6B", help="--backend trl only")
    a = p.parse_args(argv[1:])
    cfg.apply_cli(a)

    if a.backend == "trl":
        # TRL owns the loop (rollouts via rollout_func) — hand it the whole run.
        from .trainer import grpo_trl

        rollout_mode = "ignition-b" if a.topology == "b" else "local"
        grpo_trl.train(cfg, model=a.model, steps=a.steps, rollout_mode=rollout_mode)
        return 0

    base_run = cfg.run_id
    for step in range(a.steps):
        cfg.run_id = f"{base_run}-{step:03d}"
        print(f"\n=== step {step}  policy_version={cfg.policy_version}  run={cfg.run_id} ===")

        if a.topology == "a":
            run_path = controller.run(cfg)
        elif a.topology == "b":
            run_path = _run_topology_b(cfg)
        else:
            from .driver import offline
            run_path = offline.run(cfg)

        report = grpo_step.run(run_path)
        print(json.dumps({k: report[k] for k in
                          ("rows", "groups", "nonzero_advantage_groups", "pass_rate",
                           "mean_reward", "stub_loss")}, indent=2))

        cfg.policy_version += 1
        reload_inference(cfg.policy_version)

    return 0


def _run_topology_b(cfg: RunConfig) -> str:
    import os

    from ignition_sandbox import Client
    from .driver import topology_b
    from .harness.rollout import TASKS_ROOT
    from .policy import OpenAICompatPolicy, OraclePolicy
    from .trajectory import write_jsonl

    client = Client()
    results = []
    for task in cfg.tasks:
        for s in range(cfg.group_size):
            pol = (OraclePolicy(TASKS_ROOT / task)
                   if cfg.inference_url.startswith("oracle://")
                   else OpenAICompatPolicy(cfg.inference_url, cfg.inference_model, cfg.inference_token))
            results.append(topology_b.run_one(client, cfg, pol, task, s))
    os.makedirs(cfg.out_dir, exist_ok=True)
    path = os.path.join(cfg.out_dir, f"{cfg.run_id}.jsonl")
    write_jsonl(path, results)
    return path


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
