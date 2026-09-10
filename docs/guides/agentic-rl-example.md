# Agentic RL (RLVR) on Ignition — runbook

**Status:** worked example, matches `examples/agentic-rl/` (`swe-mini-rl`)
**Design rationale:** [agentic-rl-on-ignition](../design/agentic-rl-on-ignition.md)
**What is built vs not:** [STATUS.md](../design/STATUS.md)

This walks the `examples/agentic-rl/` package from a hermetic dry run to a real
(if un-optimized) RL loop driving Ignition sandboxes as rollout environments. The
task family is six small Python bug-fix tasks; reward is the fraction of a pytest
suite that passes.

## The shape

```
OUTSIDE IGNITION                              IGNITION
  vLLM (OpenAI-compatible, GPU)               N CPU sandboxes, one per rollout
  trainer / grpo_step  ◀── trajectories ──    image: swe_mini/{harness,tasks}
  rollout controller  ───── control plane ──▶ create / :watch / terminate
  collector (HTTP)     ◀──── egress ─────────  harness PUTs {trajectory, reward}
```

Two topologies (Ignition sandboxes accept **no inbound connections**, so the
result has to leave another way):

| | **A** — `driver/controller.py` | **B** — `driver/topology_b.py` |
|---|---|---|
| agent loop | inside the sandbox (`harness/rollout.py` is PID 1) | in the driver, one `sb.run` per tool call |
| sandbox egress | required (dials inference + collector) | none |
| result via | HTTP PUT to the collector | exec (`sb.run`) stdout stream |
| use for | the training run — scales, async | bring-up, one-task debugging, hermetic tests |

## 0. Hermetic — no GCP, no Docker, no LLM

```bash
cd examples/agentic-rl
make venv          # .venv + editable install of the package and the SDK
make test          # 20 tests: verify contract, agent, controller vs a fake, GRPO math
make smoke-offline # runs the harness locally (OraclePolicy) -> trajectories -> GRPO batch
```

`make smoke-offline` writes `trajectories/offline.jsonl` and
`training_batch/offline.jsonl` + `.summary.json`. Expect ~12/18 solved (the
"mixed" policy solves even samples, perturbs odd ones), and every group with
non-zero advantage spread.

## 1. Deploy Ignition and build the env image

Deploy a dev Ignition per [ignition-implementation](ignition-implementation.md).
Then build + push + admit the environment image:

```bash
cd examples/agentic-rl
export IMAGE_REGISTRY=us-central1-docker.pkg.dev/<project>/ignition
export IGNITION_SERVER=... IGNITION_TOKEN=... IGNITION_PROJECT=...
env_image/build_and_admit.sh          # docker build + push + POST /v1/projects/{p}/images
export ENV_IMAGE=img_swe_mini         # or the imageId the admit step returns
```

The image is `python:3.12-slim` + `pytest` + the `swe_mini` package. Tasks are
baked in (Ignition has no dataset mounts yet) — a new task set means a rebuild.

## 2. Topology B on the dev cluster

Topology B reads observations off the exec stream, so it needs a **reachable
gateway** — the `:attach` response's `gatewayUrl` must resolve from where the
driver runs (the polling fallback returns no output and topology B can't use it).
On `dev` that means port-forwarding the gateway and configuring the API to hand
back that URL:

```bash
kubectl -n <ns> port-forward svc/ignition-gateway 8443:8080 &
# the API's IGNITION_GATEWAY_URL must point at a reachable gateway (here 127.0.0.1:8443)

make rollout-b TASK=task_0001_sum_list RUN_ID=b1   # INFERENCE_URL=oracle:// (default)
```

That creates one sandbox (`command=["sleep", ...]`), copies the task into
`/scratch/work`, runs the agent loop from the driver — each `read_file` /
`write_file` / `run_tests` is an `sb.run(...)` over the exec stream — verifies,
prints the reward, and tears the sandbox down.

Point it at a real model:

```bash
export INFERENCE_URL=https://<vllm-host>/v1
export INFERENCE_TOKEN=...            # plain env is fine for topology B (driver-side)
export INFERENCE_MODEL=Qwen2.5-7B-Instruct
make rollout-b TASK=task_0004_flatten RUN_ID=b2
```

## 3. Topology A end to end

Topology A needs (a) the sandbox to reach your inference endpoint and (b) a
**collector reachable from GKE pod egress** — a small standing service:

```bash
# somewhere with a public HTTPS URL / ingress:
python -m swe_mini.driver.collector --port 8900 --token "$COLLECTOR_TOKEN"
```

Store the inference and collector tokens in Secret Manager and pass their ids —
Ignition injects them as env via `secretRefs`, so nothing sensitive touches the
image or plain env:

```bash
export ENV_IMAGE=img_swe_mini
export INFERENCE_URL=https://<vllm-host>/v1
export INFERENCE_TOKEN_SECRET=projects/<p>/secrets/vllm-token
export COLLECTOR_URL=https://<collector-host>
export COLLECTOR_TOKEN_SECRET=projects/<p>/secrets/collector-token

make rollout-a RUN_ID=demo GROUP_SIZE=4        # 6 tasks x 4 samples = 24 sandboxes
python -m swe_mini.trainer.grpo_step --run demo
```

The controller bounds concurrency (`MAX_INFLIGHT`, default 16), retries admission
races / transient API errors with backoff, treats a `ConflictError` as an
idempotency replay (re-fetch the trajectory by key), and **always** terminates
the sandbox. Output: `trajectories/demo.jsonl`, then `training_batch/demo.jsonl`.

## 4. The outer loop

```bash
# hermetic dry run of the plumbing (local rollouts):
INFERENCE_URL=oracle:// python -m swe_mini.train --steps 3 --topology offline --run-id loop
# on a live cluster:
python -m swe_mini.train --steps 3 --topology a --run-id loop
```

Each step: rollouts → GRPO batch → `policy_version += 1` → `reload_inference()`
(a no-op hook). Ignition appears only in the rollout step (`--topology a`/`b`).

## 5. The real GRPO step — `trl.GRPOTrainer`

`swe_mini/trainer/grpo_step.py` is the stub (batch + reported loss, no
optimizer). `swe_mini/trainer/grpo_trl.py` is the real thing, wired to
`train.py` as `--backend trl`:

```bash
pip install -e '.[train]'            # torch, transformers, trl>=1.0, datasets

# vLLM must expose exact token ids + logprobs so what the harness records lines
# up with what the trainer optimizes:
vllm serve Qwen/Qwen3-0.6B --return-tokens-as-token-ids \
    --logprobs-mode processed_logprobs --max-logprobs -1

export INFERENCE_URL=http://localhost:8000/v1 INFERENCE_MODEL=Qwen/Qwen3-0.6B
python -m swe_mini.train --backend trl --topology b --model Qwen/Qwen3-0.6B --steps 20
```

It uses TRL's `rollout_func` hook: **TRL owns the optimizer and the
group-relative advantage; we own generation (the agent harness on Ignition) and
the reward (the in-sandbox verifier).** Per step, for each task TRL asks for
`num_generations` rollouts; `grpo_trl` runs them (`--topology b` → real Ignition
sandboxes, else locally), flattens each multi-turn trajectory into
`(prompt_ids, completion_ids, logprobs)` (assistant tokens only — observations
are not trained on), and returns them plus the verified `reward`. TRL groups,
computes advantages, and does the policy step.

Verified end to end against `trl==1.13.0` with a tiny model on CPU (mocked
rollout); a real run needs a GPU and the vLLM flags above.

**Still to do for a production loop:**
- `oracle://` / `random://` policies have no logprobs — the trl backend requires
  a real vLLM endpoint.
- Weight sync: TRL's colocated vLLM (`use_vllm=True, vllm_mode="colocate"`) or
  the server + NCCL path handles pushing new weights to inference.
- Staleness: trajectories are tagged with `policy_version`; apply an off-policy
  correction / cutoff for rollouts that land a version late.
- For an agent that must run its *own* loop unchanged, see TRL's experimental
  `AsyncGRPOTrainer` + `HarnessRolloutWorker` (black-box / loop-owning) path.

## Operational notes

- **Cold start.** No warm pool is on by default (`IGNITION_MIN_WARM=0`). For a
  run, have an operator raise it on the CPU pool; keep the image lean.
- **Quota.** Parallel `CreateSandbox` is a serializable admission transaction
  with per-project quota. Cap `MAX_INFLIGHT` and request headroom before a big
  fan-out.
- **Environments are CPU-only.** The GPU path is one sandbox per node; the GPU
  belongs with the trainer and inference, not the environment.
- **Episode budget.** `maximum_runtime_seconds` (≤ 86400) is the hard backstop;
  `idle_seconds` is 0 for topology A because the agent pauses to think.
