# Running agentic RL (RLVR) environments on Ignition

**Goal:** use an Ignition sandbox as the *environment / rollout worker* for RL from
verifiable rewards — an LLM agent runs code and commands inside the sandbox, a
verifier scores the attempt, and the trajectory + reward flow back to a trainer
that lives **outside** Ignition.

This plan is grounded in how sandbox providers and RL frameworks actually wire
this up (see Sources at the end), then mapped onto what Ignition ships today
(`docs/design/STATUS.md`).

- **Worked example:** [`examples/agentic-rl/`](../../examples/agentic-rl/) — a
  runnable RLVR loop (six code-fix tasks, both topologies, a GRPO batch stub).
- **Runbook:** [`docs/guides/agentic-rl-example.md`](../guides/agentic-rl-example.md).

---

## 1. What Ignition gives you, and the constraints that shape the design

| Ignition fact (from STATUS.md / api-contract) | Consequence for an RL environment |
|---|---|
| A sandbox is one container image + one supervised main process, gVisor-isolated, CPU-only or **one whole L4** | One sandbox = one rollout / one task attempt. Bake the harness + verifier into the image. |
| **No inbound connections ever** (`network.internetAccess` is outbound-only, "never inbound; GCP networking enforces it") | The trainer/driver cannot call *into* the sandbox. Either the driver drives the sandbox over the exec data plane (outbound WebSocket from the driver), or the sandbox process dials *out* to your inference + collector endpoints. |
| No memory snapshot, no `create_image_from_filesystem`, no fork ("SESSION memory snapshots — out of scope") | Per-rollout isolation = a fresh sandbox from a pre-baked image, not a restore-from-snapshot. Put everything you can in the image. |
| No file-copy API; `run(stream=False)` returns only the exit code ("The API has no output-read endpoint outside the gateway stream") | Results leave the sandbox one of two ways: (a) parsed from the stdout stream, or (b) the sandbox pushes them to GCS / an HTTP collector over egress. Prefer (b). |
| Exec replay buffer is in-memory, no offset-based reconnect ("Durable exec spool ... DEFERRED") | A dropped WebSocket mid-turn loses that stream. Don't make reward delivery depend on an unbroken stdout stream. |
| GPU path schedules **one tenant sandbox per `g2-standard-8` node**, via Cluster Autoscaler | GPU fan-out is slow and quota-bound. Keep the GPU on the trainer/inference side. Run environments **CPU-only** — that is where real parallelism is. |
| Admission is a serializable transaction with per-project quota | Thousands of parallel `CreateSandbox` calls need quota headroom, a client-side concurrency cap, and backoff on 409 / quota errors. |
| No dataset / artifact mounts yet ("Read-only dataset mounts — PROPOSED") | Task data (repo, tests, fixtures) is baked into the image or fetched over egress at rollout start. |
| Timeouts: `startupSeconds` ≤ 600, `maximumRuntimeSeconds` ≤ 86400 (24 h, hard kill), `idleSeconds` ≤ 3600 | Set `maximumRuntimeSeconds` to your episode budget + margin. Set `idleSeconds: 0` for topology A (the agent may pause to think). |
| `secretRefs` → Secret Manager, injected as env at Pod create, never stored in SQL | Ship the inference token / collector token this way. Never bake tokens into the image or pass them as plaintext env. |
| `CreateSandbox` has a plain `environment` map (both SDKs' `env=`/`o.env` on sandbox create) alongside `secretRefs`, applied to the container regardless of `nativeEntrypoint`; an `IGNITION_`-prefixed key is rejected, not silently dropped | Non-secret per-rollout config (`TASK_ID`, `RUN_ID`, `INFERENCE_URL`, ...) can travel as plain `environment`, or as `args` on the sandbox's main process — either works. Tokens still go via `secretRefs`. |
| Image admission (`POST /v1/projects/{project}/images`) pins a registry ref to a digest | Free reproducibility: admit once per image build, schedule by digest. |
| Warm pool is implemented but off (`IGNITION_MIN_WARM=0` in every overlay) | Cold start today = image pull + Pod create. Ask the operator to set `IGNITION_MIN_WARM>0` (CPU pool) for the duration of a training run. |

---

## 2. Reference architecture

```
        ┌────────────────────────────  OUTSIDE IGNITION  ────────────────────────────┐
        │                                                                            │
        │   vLLM inference (GPU)          Trainer: GRPO/PPO (GPU)                     │
        │   OpenAI-compatible API   ◀──▶  replay buffer  ◀── weight sync, policy_version++
        │        ▲                              ▲                                     │
        │        │ turns                        │ trajectories + rewards              │
        │        │                       ┌──────┴───────────┐                         │
        │        │                       │ Rollout controller│  (the "driver")        │
        │        │                       │  - task queue     │                        │
        │        │                       │  - MAX_INFLIGHT   │                        │
        │        │                       │  - ignition-sandbox SDK                    │
        │        │                       └──────┬───────────┘                         │
        └────────┼──────────────────────────────┼─────────────────────────────────────┘
                 │ egress (topology A)          │ Ignition control plane: create / :watch / terminate
                 │                              ▼
        ┌────────┴──────────────────────────────────────────  IGNITION  ─────────────┐
        │   N CPU sandboxes (one per rollout), gVisor, ephemeral /scratch            │
        │   image = task files + agent harness (tool loop) + verify.py               │
        └───────────────────────────────────────────────────────────────────────────┘
```

### Two topologies — pick based on phase

**Topology A — in-sandbox agent (recommended for the training run; SkyRL/Harbor-style)**

- The sandbox's **main process is the agent**: `python -m rollout`, given
  `INFERENCE_URL`, `COLLECTOR_URL`, `TASK_ID`, `POLICY_VERSION` as plain
  `environment` or argv (either works), tokens via `secretRefs`.
- `network.internetAccess: ENABLED`. Each turn the harness calls `INFERENCE_URL`,
  executes the tool call locally in `/scratch`, loops; at the end it runs
  `verify.py` in-process and `POST`s `{trajectory, reward, metrics, policy_version}`
  to `COLLECTOR_URL`, then exits 0.
- Control plane does only: `create` (idempotency key = `run_id:task_id:policy_version`),
  `:watch` until terminal, read exit code, `terminate`.
- **Pros:** minimal control-plane chatter, verifier co-located, matches the
  async producer/consumer model. **Cons:** sandbox needs egress to your infra;
  reward integrity rests on the push (make it idempotent + re-fetchable).

**Topology B — driver-driven sandbox (bring-up and debugging)**

- Sandbox image is just the tool environment + `verify.py`. Main process is an
  idle supervisor (a `sleep infinity` / REPL).
- The **rollout controller runs the agent loop**: for each turn,
  `sb.run(["bash","-lc", action])`, parse the observation from stdout, call
  inference itself, decide the next action. Reward = `sb.run(["python","verify.py"])`,
  parse a sentinel-wrapped JSON blob from stdout.
- **Pros:** sandbox needs zero egress; trajectory is assembled by trusted code;
  trivial to swap policies. **Cons:** every turn is a gateway round trip (latency);
  throughput bounded by the exec plane; a dropped WS loses the turn.

> Start with **B** to prove one task end to end, switch to **A** for scale.

---

## 3. Build the environment image

`Dockerfile` skeleton (CPU; add `nvidia/cuda` base only if the verifier itself needs a GPU — usually it does not):

```dockerfile
FROM python:3.12-slim
RUN pip install --no-cache-dir <agent-harness-deps> pytest   # pin everything
WORKDIR /env
COPY harness/ /env/harness/          # the tool loop (topology A) — omit for B
COPY verify.py /env/verify.py        # the verifier
COPY tasks/ /env/tasks/              # task shard, OR a fetch script run at start
ENV PYTHONUNBUFFERED=1
# Topology A: agent is PID 1.  Topology B: `command` is set on CreateSandbox instead.
ENTRYPOINT ["python", "-m", "harness.rollout"]
```

**Verifier output contract** — `verify.py` writes exactly one line to stdout:

```
---IGN-RESULT--- {"reward": 0.0, "passed": false, "detail": {"tests_passed": 3, "tests_total": 7}}
```

and (topology A) the harness also `POST`s the full record to `COLLECTOR_URL`.

**Image hygiene**
- One image per *task family*; select the specific task at runtime via `TASK_ID`.
  Keeps the Artifact Registry image count small and GKE image-streaming cache hot.
- Rebuild only when the harness or verifier changes, not per task.
- Push under the Artifact Registry sandbox prefix, then admit:
  `POST /v1/projects/{project}/images` with the `sourceRef` → returns a digest-pinned `imageId`.
- Keep layers few and small — cold start is image-pull-bound until a warm pool exists.

---

## 4. The rollout controller (the driver)

Uses `sdks/python` (`ignition-sandbox`). One `asyncio`/thread task per rollout,
gated by a semaphore.

```python
from ignition_sandbox import Client
from ignition_sandbox.errors import ConflictError, APIError, TimeoutError

MAX_INFLIGHT = 48          # CPU sandboxes; raise once quota + latency are measured
EPISODE_BUDGET_S = 1800

def rollout(client: Client, run_id: str, task_id: str, policy_version: int):
    key = f"{run_id}:{task_id}:{policy_version}"
    sb = None
    try:
        sb = client.sandboxes.create(
            image=ENV_IMAGE_ID,
            # Non-secret config: plain `environment` (below) or argv, either
            # works. Tokens always go via secretRefs, never here.
            command=["python", "-m", "harness.rollout"],
            environment={"TASK_ID": task_id,
                         "POLICY_VERSION": str(policy_version),
                         "INFERENCE_URL": INFERENCE_URL,
                         "COLLECTOR_URL": COLLECTOR_URL},
            accelerator="NONE",
            cpu_milli=2000, memory_mib=4096,
            internet=True,                         # topology A; False for B
            secret_refs=[{"secretId": INFER_TOKEN_SECRET, "version": "latest",
                          "environmentName": "INFERENCE_TOKEN"}],
            labels={"run": run_id, "task": task_id, "pv": str(policy_version)},
            maximum_runtime_seconds=EPISODE_BUDGET_S,
            idle_seconds=0,
            startup_seconds=300,
            idempotency_key=key,
            wait=True, wait_timeout=EPISODE_BUDGET_S + 120,
        )
        # Topology A: just wait for it to finish and go get the result.
        for snap in sb.watch():
            if snap.is_terminal:
                break
        proc = next(iter(sb.processes.list()), None)
        exit_code = proc.refresh().exit_code if proc else None
        record = collector.fetch(key)              # idempotent; keyed by the same key
        if record is None or exit_code != 0:
            return failed_rollout(task_id, reason=f"exit={exit_code}, record={record is not None}")
        return record                              # {trajectory, reward, metrics, policy_version}

    except ConflictError:                          # idempotency replay / admission race
        return None                                # let the outer loop re-poll by key
    except (TimeoutError, APIError) as e:
        return retryable(task_id, e)               # bounded exp backoff + jitter, 1 retry on fresh sb
    finally:
        if sb is not None:
            try: sb.terminate(wait=False)
            except APIError: pass
```

Topology B replaces the `for snap in sb.watch()` block with the turn loop:

```python
        obs = INITIAL_PROMPT
        for turn in range(MAX_TURNS):
            action = policy(obs, policy_version)               # your inference call
            res = sb.run(["bash", "-lc", action])              # streams stdio via gateway
            obs = tail(res.stdout_captured_by_your_wrapper)    # or re-read /scratch state
            trajectory.append((obs, action))
            if done(obs): break
        score = parse_sentinel(sb.run(["python", "/env/verify.py"]))
```

**The controller is the adapter** between "an Ignition sandbox rollout" and your
RL framework's `Env` interface — implement `env.reset()` → `create`,
`env.step()` → `sb.run` (B) or n/a (A), `env.rollout()` → the function above.
Frameworks that already expect this shape: **verifiers** (Prime Intellect),
**SkyRL** / **SkyRL-Agent**, **veRL**, **rLLM**, **OpenPipe ART**, **TRL GRPO**.

**Controller must emit:** create→ready latency (server also emits
`ignition_sandbox_stage_latency_seconds`), rollout wall time, verifier pass rate,
and a failure taxonomy (startup timeout / non-zero exit / missing record / quota).

---

## 5. Trainer + inference (outside Ignition — Ignition is not in this loop)

- **Inference:** vLLM on your GPUs, OpenAI-compatible. For topology A it must be
  reachable from GKE Pod egress — public HTTPS + bearer token, or a VPC path.
- **Trainer:** GRPO or PPO. Async producer/consumer: the rollout controller fills
  the replay buffer, the trainer drains it, does the update, bumps
  `policy_version`, and reloads inference weights (or in-flight NCCL sync if your
  stack supports it).
- **Staleness:** sandboxes launched under version *N* may land after *N+1*. Tag
  every trajectory with `policy_version` and apply your off-policy correction or
  discard rule. Ignition has no "pause and resume rollout" — a stale episode
  either completes or is killed by `maximumRuntimeSeconds`.

---

## 6. Bring-up milestones

1. **One sandbox, by hand.** `ignitionctl exec` into an env image, run one task,
   eyeball `verify.py` output. (topology B, 1 task)
2. **Controller, one task, end to end.** Trajectory + reward reach a local collector.
3. **Fan out to `MAX_INFLIGHT=16`** on one task family. Measure create latency,
   quota behavior, failure rate. Decide A vs B for the run.
4. **Wire to the RL framework's replay buffer.** Do one trainer step with a frozen
   policy to prove the data path (no learning yet).
5. **Short real run** — ~100 steps on a small task set. Watch the pass-rate curve move.
6. **Scale:** raise `MAX_INFLIGHT`, add task diversity, tune episode budget and
   timeouts, request a warm CPU pool and a quota bump.

---

## 7. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Cold-start variance (no warm pool today) | Operator sets `IGNITION_MIN_WARM>0` on the CPU pool for the run; pre-admit the image; keep it lean. |
| Per-project admission quota caps parallel creates | Request a quota bump; cap `MAX_INFLIGHT`; exponential backoff + jitter on 409 / quota. |
| No inbound to the sandbox | Use topology A (sandbox dials out) or B (driver drives via exec). Never try to run an action server inside the sandbox. |
| Lost stdout stream (in-memory replay, no reconnect) | Deliver reward via push-to-collector (A), keyed by the idempotency key, idempotent and re-fetchable — not via stdout parsing. |
| 24 h hard kill / idle timeout ends an episode mid-rollout | Set `maximumRuntimeSeconds` = episode budget + margin; `idleSeconds: 0` when the agent has long think pauses. |
| GPU scarcity | Keep environments CPU-only. GPU is for the trainer and inference only. |
| Secret leakage | `secretRefs` → Secret Manager. No tokens in the image or plaintext env. |
| Reproducibility | Digest-pinned `imageId` (admission does this); log `policy_version`; seed the agent and the task sampler. |
| Broad egress from `internetAccess: ENABLED` | The rollout can reach the whole internet. Constrain what the harness is allowed to contact; review before enabling for untrusted task code. Note the image-resolver has no registry-host allowlist yet (STATUS "Security gap") — control-plane side, but track it. |
| `/scratch` lost on node loss | Fine for a single rollout; never rely on it across sandboxes. |

---

## Sources

- [Sandboxes for Reinforcement Learning — Beam Cloud](https://www.beam.cloud/blog/sandboxes-reinforcement-learning) — build-once / snapshot / restore-per-rollout / teardown; reward sources; trainer talks to rollouts via small payloads.
- [When LLMs Grow Hands and Feet: How to Design our Agentic RL Systems? — Jiachen Liu](https://amberljc.github.io/blog/2025-09-05-agentic-rl-systems.html) — decoupled RL framework / execution environments / agent layer; producer–consumer rollout vs training; container-sandbox pitfalls.
- [Training frontier knowledge-work agents: a 397B RL training guide with SkyRL — Mercor](https://www.mercor.com/blog/training-frontier-knowledge-work-agents-a-397b-rl-training-guide-with-skyrl/) — Harbor Trial lifecycle: env start → agent.run() → verify in-sandbox → teardown; per-trial Modal sandbox from an image; reward joins the trajectory back to the trainer.
- [SkyRL-Agent: Efficient RL Training for Multi-turn LLM Agents (arXiv 2511.16108)](https://arxiv.org/pdf/2511.16108) and [SkyRL agentic RL systems survey context](https://amberljc.github.io/blog/2025-09-05-agentic-rl-systems.html) — 80–100 containers per replica, crun, async rollout/training separation.
- [Gymnasium — Training an Agent](https://gymnasium.farama.org/main/introduction/train_agent/) and [Create a Custom Environment](https://gymnasium.farama.org/introduction/create_custom_env/) — the `reset()` / `step()` / reward interface the controller adapter should expose.
- [awesome-RLVR (opendilab)](https://github.com/opendilab/awesome-RLVR) and [HuggingEnvs / RL_Envs_101](https://github.com/adithya-s-k/RL_Envs_101) — RLVR framework and environment-building survey; `verifiers`, veRL, rLLM, ART.
- Ignition: `docs/design/STATUS.md`, `docs/design/ignition-api-contract.md`, `sdks/python/README.md`.
