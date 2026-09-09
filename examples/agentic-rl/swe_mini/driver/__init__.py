"""Out-of-cluster side: rollout orchestration against the Ignition control plane.

- :mod:`swe_mini.driver.collector`   — HTTP sink the in-sandbox harness PUTs
  trajectories to (topology A).
- :mod:`swe_mini.driver.controller`  — topology A: fan a batch of rollouts out as
  CPU sandboxes, gather their trajectories, always tear down.
- :mod:`swe_mini.driver.topology_b`  — topology B: keep the agent loop in the
  driver and drive a bare sandbox over the exec stream. No sandbox egress.
- :mod:`swe_mini.driver.config`      — one config object from env + CLI.
"""
