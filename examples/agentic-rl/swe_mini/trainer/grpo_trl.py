"""The real GRPO training step — `trl.GRPOTrainer` with Ignition rollouts.

This closes the loop the rest of the example leaves as a stub (`grpo_step.py`).
It uses TRL's ``rollout_func`` hook: **TRL owns the optimizer and the
group-relative advantage; we own generation (the agent harness) and the reward
(the verifier).**

    prompts ──▶ rollout_func: run N agent rollouts per task on Ignition (or
                locally), each = a multi-turn trajectory, verified in-sandbox
            ──▶ return {prompt_ids, completion_ids, logprobs, reward, task_id}
    TRL     ──▶ group by prompt, advantage = (r - mean)/std, GRPO policy step

Requirements for a real run:

- `pip install -e '.[train]'`  (torch, transformers, trl>=0.27, datasets) + a GPU.
- A vLLM inference server started so the recorded tokens/logprobs line up exactly
  with what the trainer optimizes:

      vllm serve <model> --return-tokens-as-token-ids \
          --logprobs-mode processed_logprobs --max-logprobs -1

  Point `INFERENCE_URL` at it (the offline `oracle://` / `random://` policies do
  not produce logprobs and are rejected by this backend).

See docs/guides/agentic-rl-example.md "Closing the loop".
"""

from __future__ import annotations

import argparse
import tempfile

from ..driver.config import RunConfig
from ..harness.agent import SYSTEM_PROMPT
from ..harness.rollout import TASKS_ROOT, rollout_once
from ..policy import OpenAICompatPolicy
from ..trajectory import Trajectory


def _require_train_deps() -> None:
    missing = []
    for mod in ("torch", "transformers", "trl", "datasets"):
        try:
            __import__(mod)
        except ImportError:
            missing.append(mod)
    if missing:
        raise SystemExit(
            "the trl backend needs the [train] extra — "
            f"`pip install -e '.[train]'` (missing: {', '.join(missing)})"
        )


def task_prompt_messages(task_id: str) -> list[dict]:
    prompt = (TASKS_ROOT / task_id / "PROMPT.md").read_text(encoding="utf-8")
    return [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": prompt},
    ]


# --------------------------------------------------------------------------- #
# trajectory -> (prompt_ids, completion_ids, logprobs)
# --------------------------------------------------------------------------- #
def _token_to_id(tokenizer, tok: str) -> int:
    if tok.startswith("token_id:"):
        return int(tok.split(":", 1)[1])
    return tokenizer.convert_tokens_to_ids(tok)


def flatten_trajectory(traj: Trajectory, tokenizer, prompt_messages: list[dict]):
    """Flatten a multi-turn agent trajectory into one training sequence: the
    completion is the concatenation of the assistant turns (the agent's
    actions); observations are not trained on. Every turn must carry per-token
    ids + logprobs from the inference endpoint."""
    prompt_text = tokenizer.apply_chat_template(
        prompt_messages, tokenize=False, add_generation_prompt=True
    )
    prompt_ids = tokenizer(prompt_text, add_special_tokens=False)["input_ids"]

    completion_ids: list[int] = []
    logprobs: list[float] = []
    for turn in traj.turns:
        c = turn.completion
        if not (c.tokens and c.token_logprobs) or len(c.tokens) != len(c.token_logprobs):
            raise ValueError(
                "trl backend requires per-token logprobs+tokens on every turn "
                "(run vLLM with --return-tokens-as-token-ids and set INFERENCE_URL)"
            )
        completion_ids.extend(_token_to_id(tokenizer, t) for t in c.tokens)
        logprobs.extend(float(x) for x in c.token_logprobs)
    return prompt_ids, completion_ids, logprobs


# --------------------------------------------------------------------------- #
# rollout
# --------------------------------------------------------------------------- #
def _run_rollout(cfg: RunConfig, task_id: str, sample: int, mode: str, client=None) -> Trajectory:
    if mode == "ignition-b":
        from ..driver import topology_b

        policy = OpenAICompatPolicy(cfg.inference_url, cfg.inference_model, cfg.inference_token)
        return topology_b.run_one(client, cfg, policy, task_id, sample)

    # local: run the harness in a temp dir, still calling the real INFERENCE_URL
    policy = OpenAICompatPolicy(cfg.inference_url, cfg.inference_model, cfg.inference_token)
    with tempfile.TemporaryDirectory() as wd:
        return rollout_once(
            task_id, wd, policy, run_id=cfg.run_id,
            policy_version=cfg.policy_version, sample=sample, max_turns=cfg.max_turns,
        )


def make_rollout_func(cfg: RunConfig, prompt_to_task: dict[str, str], tokenizer, mode: str):
    """TRL's dataloader already repeats each prompt ``num_generations`` times, so
    ``prompts`` arrives pre-expanded and we return exactly one rollout per entry.
    Consecutive entries share a prompt — that is the GRPO group."""
    client = None
    if mode == "ignition-b":
        from ignition_sandbox import Client

        client = Client()

    def rollout_func(prompts, trainer) -> dict:
        out: dict[str, list] = {
            "prompt_ids": [], "completion_ids": [], "logprobs": [],
            "reward": [], "task_id": [],
        }
        for i, messages in enumerate(prompts):
            task_id = prompt_to_task[messages[-1]["content"]]
            traj = _run_rollout(cfg, task_id, i, mode, client)
            pids, cids, lps = flatten_trajectory(traj, tokenizer, list(messages))
            out["prompt_ids"].append(pids)
            out["completion_ids"].append(cids)
            out["logprobs"].append(lps)
            out["reward"].append(traj.reward)
            out["task_id"].append(task_id)
        return out

    return rollout_func


def reward_func(completions, reward, **kwargs) -> list[float]:
    """The verifier already scored each rollout in-sandbox; ``reward`` is
    forwarded here from rollout_func's extra fields. Just pass it through."""
    return [float(r) for r in reward]


# --------------------------------------------------------------------------- #
# train
# --------------------------------------------------------------------------- #
def train(cfg: RunConfig, *, model: str, steps: int = 1, num_generations: int = 8,
          rollout_mode: str = "local", out_dir: str = "grpo_out",
          grpo_config_kwargs: dict | None = None) -> str:
    _require_train_deps()
    from datasets import Dataset
    from transformers import AutoTokenizer
    from trl import GRPOConfig, GRPOTrainer

    if not cfg.inference_url.startswith(("http://", "https://")):
        raise SystemExit("set INFERENCE_URL to a vLLM endpoint; oracle:// has no logprobs")

    tokenizer = AutoTokenizer.from_pretrained(model)
    rows = {"prompt": [], "task_id": []}
    for t in cfg.tasks:
        rows["prompt"].append(task_prompt_messages(t))
        rows["task_id"].append(t)
    dataset = Dataset.from_dict(rows)
    prompt_to_task = {m[-1]["content"]: t for m, t in zip(rows["prompt"], rows["task_id"])}

    rollout = make_rollout_func(cfg, prompt_to_task, tokenizer, rollout_mode)

    args = GRPOConfig(
        output_dir=out_dir,
        num_generations=num_generations,
        per_device_train_batch_size=num_generations,
        gradient_accumulation_steps=1,
        max_completion_length=1024,
        max_steps=steps,
        learning_rate=1e-6,
        beta=0.0,
        logging_steps=1,
        log_completions=True,
        **(grpo_config_kwargs or {}),
    )
    trainer = GRPOTrainer(
        model=model,
        args=args,
        train_dataset=dataset,
        reward_funcs=[reward_func],
        rollout_func=rollout,
        processing_class=tokenizer,
    )
    trainer.train()
    trainer.save_model(out_dir)
    print(f"saved GRPO-updated model -> {out_dir}")
    return out_dir


def main(argv: list[str]) -> int:
    cfg = RunConfig.from_env()
    p = argparse.ArgumentParser(description="Real GRPO step via trl.GRPOTrainer + Ignition rollouts.")
    cfg.add_cli(p)
    p.add_argument("--model", default="Qwen/Qwen3-0.6B")
    p.add_argument("--steps", type=int, default=1)
    p.add_argument("--num-generations", type=int, default=8)
    p.add_argument("--rollout-mode", choices=["local", "ignition-b"], default="local")
    p.add_argument("--out-dir", default="grpo_out")
    a = p.parse_args(argv[1:])
    cfg.apply_cli(a)
    train(cfg, model=a.model, steps=a.steps, num_generations=a.num_generations,
          rollout_mode=a.rollout_mode, out_dir=a.out_dir)
    return 0


if __name__ == "__main__":
    import sys

    raise SystemExit(main(sys.argv))
