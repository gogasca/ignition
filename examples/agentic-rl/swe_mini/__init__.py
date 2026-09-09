"""swe-mini-rl — agentic RL (RLVR) on Ignition sandboxes.

Layout:

- ``swe_mini.harness``  — runs *inside* the sandbox: an LLM agent tool-loop plus
  the pytest verifier. Standard-library only. Entry point for topology A is
  ``python -m swe_mini.harness.rollout``.
- ``swe_mini.tasks``    — the "swe-mini" code-fix task family (data, baked into
  the sandbox image).
- ``swe_mini.driver``   — runs *outside* Ignition: the rollout controller
  (topology A) and the driver-driven loop (topology B), the trajectory collector,
  and the policy adapters.
- ``swe_mini.trainer``  — a data-path stub: group-relative advantage + a training
  batch in the shape ``trl.GRPOTrainer`` / ``verifiers`` expect. No optimizer.
- ``swe_mini.trajectory`` — the record shared by all three.
"""

__version__ = "0.1.0"
