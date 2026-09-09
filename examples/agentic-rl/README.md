# swe-mini-rl — agentic RL (RLVR) on Ignition sandboxes

A worked example of using an **Ignition sandbox as the environment / rollout
worker** for reinforcement learning from verifiable rewards: an LLM agent fixes a
bug inside the sandbox, a pytest verifier scores the attempt, and the trajectory
+ reward flow back to a trainer that runs *outside* Ignition.

The task family ("swe-mini") is six small Python bug-fix tasks with pytest suites
as the ground truth. Reward is dense: fraction of tests passing.

```
swe_mini/
  harness/      runs INSIDE the sandbox: agent tool-loop + pytest verifier (stdlib only)
  tasks/        the six bug-fix tasks (baked into the sandbox image)
  policy.py     OpenAICompatPolicy (vLLM) | OraclePolicy | RandomEditPolicy
  driver/       runs OUTSIDE Ignition: rollout controller, collector, topology-B loop
  trainer/      data-path stub: GRPO advantages + a trl/verifiers-shaped batch (no optimizer)
  train.py      the outer loop: rollouts -> batch -> (optimizer TODO) -> version bump
```

## The two topologies

Ignition sandboxes **take no inbound connections**, so the rollout result has to
leave another way:

| | Topology A (`driver/controller.py`) | Topology B (`driver/topology_b.py`) |
|---|---|---|
| Agent loop runs | inside the sandbox (`harness/rollout.py` is PID 1) | in the driver, one `sb.run` per tool call |
| Sandbox egress | **required** — dials inference + the collector | none |
| Result leaves via | HTTP PUT to the collector | the exec (`sb.run`) stdout stream |
| Use for | the actual training run — scales, async | bring-up, debugging one task, hermetic tests |

## Quickstart

### 1. Hermetic — no GCP, no Docker, no LLM

```bash
python -m venv .venv && .venv/bin/pip install -e '.[dev]'
.venv/bin/python -m pytest              # verify contract, oracle agent, controller vs FakeIgnition, GRPO math
make smoke-offline                      # full loop with OraclePolicy: rollouts -> GRPO batch
```

`make smoke-offline` runs the harness locally against each task with
`OraclePolicy` (which knows each fix), writes `trajectories/offline.jsonl`, then
`trainer.grpo_step` emits `training_batch/offline.jsonl` + a summary.

### 2. Topology B against a dev Ignition

Needs a deployed Ignition (`docs/guides/ignition-implementation.md`), the env
image built + admitted (`env_image/build_and_admit.sh`), and — for exec
streaming — `kubectl port-forward svc/ignition-gateway 8443:8080`.

```bash
export IGNITION_SERVER=... IGNITION_TOKEN=... IGNITION_PROJECT=...
export ENV_IMAGE=img_swe_mini            # the admitted imageId
make rollout-b TASK=task_0001_sum_list   # INFERENCE_URL=oracle:// by default
# real agent:
export INFERENCE_URL=https://your-vllm/v1 INFERENCE_TOKEN=... INFERENCE_MODEL=Qwen2.5-7B-Instruct
make rollout-b TASK=task_0004_flatten
```

### 3. Topology A end to end

Run a collector reachable from GKE pod egress (public HTTPS + token), build +
admit the env image, then:

```bash
export IGNITION_SERVER=... IGNITION_TOKEN=... IGNITION_PROJECT=...
export ENV_IMAGE=img_swe_mini
export INFERENCE_URL=https://your-vllm/v1 INFERENCE_TOKEN_SECRET=projects/.../secrets/vllm-token
export COLLECTOR_URL=https://your-collector COLLECTOR_TOKEN_SECRET=projects/.../secrets/collector-token
make rollout-a RUN_ID=demo GROUP_SIZE=4
python -m swe_mini.trainer.grpo_step --run demo
```

`INFERENCE_TOKEN_SECRET` / `COLLECTOR_TOKEN_SECRET` are Secret Manager ids;
Ignition injects them as env in the sandbox via `secretRefs` — tokens never go in
the image or in plain env.

### 4. The outer loop

```bash
INFERENCE_URL=oracle:// python -m swe_mini.train --steps 3 --topology offline --run-id loop
# against a live cluster: --topology a  (or b)
```

Each step: run rollouts → build the GRPO batch → bump `policy_version` →
`reload_inference()` (a no-op hook). Dropping a real `trl.GRPOTrainer` step and a
vLLM weight reload into `swe_mini/train.py` closes the loop — see
[`docs/guides/agentic-rl-example.md`](../../docs/guides/agentic-rl-example.md).

## What this example does not do

- **No optimizer.** `trainer/` stops at the training batch + a reported stub
  loss. The `[train]` extra (`torch`, `trl`) and the `reload_inference()` hook
  are the seams for a real step.
- **No warm pool / quota tuning.** Cold start is image-pull-bound until an
  operator raises `IGNITION_MIN_WARM` on the CPU pool.
- **Tasks are baked into the image** (no dataset mounts on Ignition yet), so a
  large task set means a rebuild.

Design rationale and the Ignition constraints that shaped this:
[`docs/design/agentic-rl-on-ignition.md`](../../docs/design/agentic-rl-on-ignition.md).
