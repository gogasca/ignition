"""Trainer side — a *data-path stub*.

It does everything up to the optimizer: load the run's trajectories, compute
group-relative (GRPO) advantages, and emit a training batch in the shape
``trl.GRPOTrainer`` / the ``verifiers`` library expect. It does not run a
gradient step — wiring a real optimizer + vLLM weight reload is the documented
next step (see docs/guides/agentic-rl-example.md, and the ``[train]`` extra).
"""
