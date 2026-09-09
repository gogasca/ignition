"""Run configuration, assembled from environment variables with CLI overrides.

Ignition connection (``IGNITION_SERVER`` / ``IGNITION_TOKEN`` / ``IGNITION_PROJECT``)
is read by the SDK itself; this object carries the RL-run knobs.
"""

from __future__ import annotations

import argparse
import os
from dataclasses import dataclass, field


def _env_list(name: str, default: list[str]) -> list[str]:
    raw = os.environ.get(name, "").strip()
    return [x for x in raw.split(",") if x] or default


ALL_TASKS = [
    "task_0001_sum_list",
    "task_0002_is_palindrome",
    "task_0003_fib",
    "task_0004_flatten",
    "task_0005_dedupe_order",
    "task_0006_binsearch",
]


@dataclass
class RunConfig:
    run_id: str = "adhoc"
    policy_version: int = 0
    tasks: list[str] = field(default_factory=lambda: list(ALL_TASKS))
    group_size: int = 4                 # rollouts per task (the GRPO group)
    max_inflight: int = 16              # concurrent sandboxes

    env_image: str = "img_swe_mini"     # admitted Artifact Registry ref / imageId
    cpu_milli: int = 2000
    memory_mib: int = 4096
    episode_seconds: int = 1800
    startup_seconds: int = 300
    collector_wait_seconds: int = 120  # how long to wait for a rollout's trajectory

    # Agent inference (passed through to the sandbox in topology A; used by the
    # driver directly in topology B).
    inference_url: str = "oracle://"
    inference_model: str = "policy"
    inference_token_secret: str = ""    # Secret Manager id -> INFERENCE_TOKEN
    inference_token: str = ""           # plaintext, topology B / local only
    max_turns: int = 12

    # Collector (topology A). If unset the controller starts a local one.
    collector_url: str = ""
    collector_token: str = ""
    collector_token_secret: str = ""    # Secret Manager id -> COLLECTOR_TOKEN

    out_dir: str = "trajectories"

    # -- construction ------------------------------------------------
    @classmethod
    def from_env(cls) -> "RunConfig":
        d = cls()
        d.run_id = os.environ.get("RUN_ID", d.run_id)
        d.policy_version = int(os.environ.get("POLICY_VERSION", d.policy_version))
        d.tasks = _env_list("TASKS", d.tasks)
        d.group_size = int(os.environ.get("GROUP_SIZE", d.group_size))
        d.max_inflight = int(os.environ.get("MAX_INFLIGHT", d.max_inflight))
        d.env_image = os.environ.get("ENV_IMAGE", d.env_image)
        d.episode_seconds = int(os.environ.get("EPISODE_SECONDS", d.episode_seconds))
        d.inference_url = os.environ.get("INFERENCE_URL", d.inference_url)
        d.inference_model = os.environ.get("INFERENCE_MODEL", d.inference_model)
        d.inference_token_secret = os.environ.get("INFERENCE_TOKEN_SECRET", d.inference_token_secret)
        d.inference_token = os.environ.get("INFERENCE_TOKEN", d.inference_token)
        d.max_turns = int(os.environ.get("MAX_TURNS", d.max_turns))
        d.collector_url = os.environ.get("COLLECTOR_URL", d.collector_url)
        d.collector_token = os.environ.get("COLLECTOR_TOKEN", d.collector_token)
        d.collector_token_secret = os.environ.get("COLLECTOR_TOKEN_SECRET", d.collector_token_secret)
        d.out_dir = os.environ.get("OUT_DIR", d.out_dir)
        return d

    def add_cli(self, p: argparse.ArgumentParser) -> None:
        p.add_argument("--run-id", default=self.run_id)
        p.add_argument("--policy-version", type=int, default=self.policy_version)
        p.add_argument("--tasks", default=",".join(self.tasks))
        p.add_argument("--group-size", type=int, default=self.group_size)
        p.add_argument("--max-inflight", type=int, default=self.max_inflight)
        p.add_argument("--env-image", default=self.env_image)
        p.add_argument("--inference-url", default=self.inference_url)
        p.add_argument("--inference-model", default=self.inference_model)
        p.add_argument("--collector-url", default=self.collector_url)
        p.add_argument("--out-dir", default=self.out_dir)

    def apply_cli(self, a: argparse.Namespace) -> "RunConfig":
        self.run_id = a.run_id
        self.policy_version = a.policy_version
        self.tasks = [t for t in a.tasks.split(",") if t]
        self.group_size = a.group_size
        self.max_inflight = a.max_inflight
        self.env_image = a.env_image
        self.inference_url = a.inference_url
        self.inference_model = a.inference_model
        self.collector_url = a.collector_url
        self.out_dir = a.out_dir
        return self

    # -- helpers ---------------------------------------------------
    def sandbox_env(self, task_id: str, sample: int, collector_url: str) -> dict[str, str]:
        """Plain (non-secret) environment for a topology-A rollout sandbox."""
        env = {
            "TASK_ID": task_id,
            "RUN_ID": self.run_id,
            "POLICY_VERSION": str(self.policy_version),
            "SAMPLE": str(sample),
            "MAX_TURNS": str(self.max_turns),
            "INFERENCE_URL": self.inference_url,
            "INFERENCE_MODEL": self.inference_model,
            "WORK_DIR": "/scratch/work",
        }
        if collector_url:
            env["COLLECTOR_URL"] = collector_url
        if self.inference_token and self.inference_url.startswith(("http://", "https://")):
            # local / non-secret path only
            env["INFERENCE_TOKEN"] = self.inference_token
        if self.collector_token and not self.collector_token_secret:
            env["COLLECTOR_TOKEN"] = self.collector_token
        return env

    def secret_refs(self) -> list[dict]:
        refs = []
        if self.inference_token_secret:
            refs.append({"secretId": self.inference_token_secret, "version": "latest",
                         "environmentName": "INFERENCE_TOKEN"})
        if self.collector_token_secret:
            refs.append({"secretId": self.collector_token_secret, "version": "latest",
                         "environmentName": "COLLECTOR_TOKEN"})
        return refs
