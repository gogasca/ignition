"""Hermetic coverage for the trl backend — the pure trajectory→training-row
logic and the dependency guard. The actual `trainer.train()` needs the [train]
extra + a GPU + vLLM and is exercised separately (see the guide)."""

from __future__ import annotations

import pytest

from swe_mini.trainer import grpo_trl
from swe_mini.trajectory import Completion, ToolCall, Trajectory, Turn


class FakeTokenizer:
    """Just enough of a HF tokenizer for flatten_trajectory."""

    def apply_chat_template(self, messages, tokenize=False, add_generation_prompt=True):
        return " ".join(m["content"] for m in messages) + (" <gen>" if add_generation_prompt else "")

    def __call__(self, text, add_special_tokens=False):
        return {"input_ids": [len(w) for w in text.split()]}

    def convert_tokens_to_ids(self, tok):
        return hash(tok) % 1000


def _turn(text, tokens, logps, obs="ok"):
    return Turn(
        prompt=[{"role": "user", "content": "p"}],
        completion=Completion(text=text, tool_call=ToolCall("write_file", {}),
                              tokens=tokens, token_logprobs=logps),
        observation=obs,
    )


def _traj(turns, reward=1.0):
    return Trajectory(key="r:t:0:0", run_id="r", task_id="task_0001_sum_list",
                      policy_version=0, sample=0, turns=turns, reward=reward, passed=reward == 1.0)


def test_flatten_aligns_ids_and_logprobs():
    tok = FakeTokenizer()
    traj = _traj([
        _turn("a", ["token_id:5", "token_id:6", "token_id:7"], [-0.1, -0.2, -0.3]),
        _turn("b", ["token_id:8", "token_id:9"], [-0.4, -0.5]),
    ])
    pids, cids, lps = grpo_trl.flatten_trajectory(traj, tok, [{"role": "user", "content": "hello"}])
    assert cids == [5, 6, 7, 8, 9]
    assert lps == [-0.1, -0.2, -0.3, -0.4, -0.5]
    assert len(pids) > 0


def test_flatten_uses_convert_tokens_to_ids_for_plain_tokens():
    tok = FakeTokenizer()
    traj = _traj([_turn("x", ["hel", "lo"], [-1.0, -2.0])])
    _, cids, lps = grpo_trl.flatten_trajectory(traj, tok, [{"role": "user", "content": "q"}])
    assert cids == [tok.convert_tokens_to_ids("hel"), tok.convert_tokens_to_ids("lo")]
    assert lps == [-1.0, -2.0]


def test_flatten_rejects_turn_without_logprobs():
    tok = FakeTokenizer()
    traj = _traj([_turn("a", None, None)])
    with pytest.raises(ValueError, match="per-token logprobs"):
        grpo_trl.flatten_trajectory(traj, tok, [{"role": "user", "content": "q"}])


def test_reward_func_passes_verifier_reward_through():
    assert grpo_trl.reward_func(completions=["x", "y"], reward=[0.4, 1.0]) == [0.4, 1.0]


def test_require_train_deps_raises_when_a_dep_is_missing(monkeypatch):
    import builtins

    real_import = builtins.__import__

    def fake_import(name, *a, **k):
        if name == "trl":
            raise ImportError("no trl")
        return real_import(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", fake_import)
    with pytest.raises(SystemExit, match=r"\[train\] extra"):
        grpo_trl._require_train_deps()


def test_rollout_func_returns_one_row_per_prompt(monkeypatch):
    """TRL passes prompts already repeated num_generations times; rollout_func
    must return exactly len(prompts) rows (regression guard)."""
    tok = FakeTokenizer()
    traj = _traj([_turn("a", ["token_id:1", "token_id:2"], [-0.1, -0.2])])
    monkeypatch.setattr(grpo_trl, "_run_rollout", lambda *a, **k: traj)

    from swe_mini.driver.config import RunConfig

    cfg = RunConfig(run_id="r", tasks=["task_0001_sum_list"], inference_url="http://x")
    msgs = grpo_trl.task_prompt_messages("task_0001_sum_list")
    prompt_to_task = {msgs[-1]["content"]: "task_0001_sum_list"}
    rf = grpo_trl.make_rollout_func(cfg, prompt_to_task, tok, mode="local")

    out = rf([msgs, msgs, msgs], trainer=None)  # 3 (pre-repeated) prompts
    assert [len(out[k]) for k in ("prompt_ids", "completion_ids", "logprobs", "reward")] == [3, 3, 3, 3]
    assert out["reward"] == [1.0, 1.0, 1.0]


def test_task_prompt_messages_shape():
    msgs = grpo_trl.task_prompt_messages("task_0003_fib")
    assert [m["role"] for m in msgs] == ["system", "user"]
    assert "fib" in msgs[1]["content"]
