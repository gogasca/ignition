# Ignition roadmap

**What Ignition is becoming:** a control-plane platform for creating sandboxes
that run **agents** (coding / tool-use loops over untrusted code) and **RL
environments** (rollout workers for RL from verifiable rewards). One API, one
admission path, one lifecycle — the shared substrate both workloads need.

**What Ignition is not:** the trainer, the optimizer, or the inference server.
For an RL run, the GRPO/PPO step and the vLLM policy stay outside Ignition;
Ignition provides the environment sandboxes and the control plane that fans them
out, meters them, and streams their I/O. See
[`examples/agentic-rl/`](../../examples/agentic-rl/) and its
[design notes](agentic-rl-on-ignition.md).

Per-feature build status is in [STATUS.md](STATUS.md). This page is the *why* and
the *order*.

## The platform thesis

An agent rollout and an RL rollout are the same shape:

```
create a sandbox from an image  →  run untrusted code in it (tools / an agent)
  →  observe it (exec stream, or a verifier)  →  score / collect  →  tear it down
```

Both need: fast create, hard isolation, per-tenant admission + quota, an
idempotent create key (one rollout = one attempt), an exec data plane, and clean
teardown. Ignition ships all of that today on GKE Sandbox (gVisor), CPU or one
L4. The roadmap is the gap between "works" and "run 10k of these an hour,
cheaply, reproducibly."

## Shipped toward it

- `ignition-api` + `ignition-controller`: auth (Google OIDC), SQL project RBAC,
  serializable admission + quota, 24h idempotency replay, sandbox / process /
  operation lifecycle, SSE watch.
- gVisor Pod per sandbox — server-owned spec, read-only root, dropped caps, no
  SA token. CPU pool or one whole L4 (`ignition-gpu-agent` attests GPU identity
  + health before `READY`).
- Managed `command`/`args` → a supervised main process with `exec` / PTY / idle
  tracking; `nativeEntrypoint` for images that own PID 1.
- Exec data plane: `ignition-api`-minted stream token, `ignition-gateway` proxies
  the attach WebSocket; Python (`run(capture=True)`) + TypeScript SDKs,
  `ignitionctl`.
- `secretRefs` (Secret Manager → Pod env), ephemeral `/scratch`, per-request
  timeouts, outbound-only networking.
- v0 image admission — pin a registry ref to a digest.
- Worked example: `examples/agentic-rl/` — sandbox-as-RL-environment, two
  topologies, a real `trl.GRPOTrainer` step.

## Near-term — make rollout fan-out cheap and reproducible

| Work | Why it matters for agents / RL envs |
|---|---|
| **Warm CPU pools** (`IGNITION_MIN_WARM > 0`) | Rollout throughput is create-latency-bound; a cold pull per attempt kills fan-out. Implemented but off in every overlay — needs a load run against the 9s p95 SLO. |
| **Read-only dataset / artifact mounts** | Ship task sets, repos, eval suites, fixtures without rebuilding the image per change. Not yet built. |
| **Signature / provenance / scan; same-region Ignition-owned copy** | Registry-host allowlist + SSRF guard + resolve timeout are in (`internal/imagecatalog/guard.go`); identity/provenance verification and a copy that removes the source-registry dependency are still open. |
| **Usage / metering ledger + reconciler** | Per-run, per-project cost accounting for large rollout batches. Not yet built. |
| **Project / Secret / Event public APIs** | Self-serve project + secret management instead of seed rows. Contract exists; not yet built. |

## Later — scale and latency

- **GKE Pod snapshot orchestration** — sub-second rollout start from a prepared
  environment snapshot instead of a full boot. GKE Pod Snapshots are GA;
  Ignition does not yet create / qualify / select them.
- **Adaptive image delivery** — secondary boot-disk cache cohorts, lazy/eager
  selection, same-region import.
- **Cloud IAP auth** — verifier + component are built; enabling it is an
  operator step, not wired into an overlay.
- **Higher-level rollout API** — a batch/collection primitive so a driver asks
  for "N rollouts of image X across these inputs" instead of N `CreateSandbox`
  calls, with server-side concurrency + retry.
- **Durable exec reconnect** — replace the in-memory replay buffer with a
  durable spool + offset-based reconnect (`ignition-ingress`, route table), so
  a long rollout can resume its exec stream past a gateway restart.
- **SDK ergonomics** — native `async` Python client, mid-session PTY resize,
  text-mode wrappers / backpressure helpers for high-volume rollout output.

## Operational hardening

- **SPIFFE/SPIRE internal identity** — a unified workload identity for
  service-to-service calls; today Google API auth and internal auth are
  separate mechanisms.
- **Cross-region DR drills, launch gates, threat-model review** — needed before
  a broader production rollout; targets in
  [production-operations](ignition-production-operations.md).

## Explicitly out of scope

- Trainer / optimizer / inference server — those are the caller's.
- Writable persistent volumes, SESSION memory snapshots.
- The custom Compute Engine runtime (`ignition-scheduler`, worker fleet, CUDA
  checkpoint/restore, …) — retained as the design of record in
  [deferred-runtime](ignition-deferred-runtime.md), built only if measured
  evidence shows GKE cannot meet a requirement.
