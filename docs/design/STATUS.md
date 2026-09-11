# Ignition status: what is built

One table per area. This is the fast answer to "is X implemented?" — the design
docs carry the detail. The [implementation guide](../guides/ignition-implementation.md)
is authoritative for exactly what is deployed and how. Where the platform is
headed — sandboxes for **agents** and **RL environments** — is in
[ROADMAP.md](ROADMAP.md).

**Status vocabulary**

| Tag | Meaning |
|---|---|
| **SHIPPED** | Built, tested, and running in the `dev` and/or `anyscale-staging` overlays on GCP project `anyscale-demo`. |
| **PARTIAL** | A usable slice is built; named gaps remain. |
| **PROPOSED** | Designed, not built. May be built later; not on the deploy path. |
| **DEFERRED** | Designed as the record for the custom Compute Engine runtime. Built only if measured evidence shows GKE cannot meet a requirement — see [deferred runtime](ignition-deferred-runtime.md). |

## Control plane and API

| Capability | Status | Notes |
|---|---|---|
| `ignition-api` — HTTP/JSON public API, auth, admission, quota, idempotency | **SHIPPED** | No Kubernetes access. |
| `ignition-controller` — reconciles sandboxes into GKE Pods | **SHIPPED** | Sole holder of Pod RBAC. CPU lifecycle verified end to end. |
| Google OIDC authentication | **SHIPPED** | Verified end to end on staging. |
| Cloud IAP authentication | **PARTIAL** | Verifier + `deploy/k8s/components/iap` component built and tested. Enabling it is an operator step (include the component, set `IGNITION_IAP_AUDIENCE`, grant `roles/iap.httpsResourceAccessor`); not enabled in any overlay. |
| SQL-backed project RBAC (`roleBindings`, last-owner guard, audit line) | **SHIPPED** | |
| Sandbox lifecycle: create / get / list / terminate / watch (SSE) | **SHIPPED** | `:watch` pushes on change via Postgres `LISTEN/NOTIFY` (10s poll backstop), stays open to terminal / disconnect / 30-min cap. |
| Process control plane: create / get / list / attach / signal / cancel | **SHIPPED** | |
| Operations: get / list / watch / cancel | **SHIPPED** | |
| Idempotency (`Idempotency-Key`, 24h replay) | **SHIPPED** | |
| Cloud SQL schema + serializable admission transaction | **SHIPPED** | Regional HA on dev; password DSN, IAM DB users are the prod path. |
| Project / Secret / Event public APIs | **PROPOSED** | API runs on seed `projects` rows; contract in [api-contract](ignition-api-contract.md). |
| Scheduler queue, GPU-lease table, transactional outbox, quota ledger, worker streams | **DEFERRED** | GKE scheduler + Cluster Autoscaler replace them. |

## Sandbox runtime

| Capability | Status | Notes |
|---|---|---|
| CPU sandbox (`accelerator: NONE`) as a gVisor Pod on `cpu-sandbox` | **SHIPPED** | Verified end to end on dev. |
| `NVIDIA_L4` GPU sandbox, one whole GPU, one sandbox per node | **SHIPPED** | Verified end to end on `anyscale-demo`: `CREATING → READY` on a real `g2-standard-8` + L4 node, `ignition.io/gpu-uuid` + `init-healthy` attested, `nvidia-smi -L` and a real `cuInit()` (`cuda-check`) succeed inside the gVisor sandbox, `terminate → FINISHED` leaves the node clean. |
| `ignition-gpu-agent` — GPU identity + health attestation, node-reuse gating | **SHIPPED** | Privileged DaemonSet on the GPU pool; `distroless/base` (glibc, to exec `nvidia-smi`), tolerates its own `gpu-reuse-pending` fence taint, reads `remapped_rows.*` for the reset signal (driver 580+). |
| `sandbox-init` — readiness probe + tenant-process supervision | **SHIPPED** | |
| Server-owned Pod spec (gVisor, read-only root, dropped caps, no SA token) | **SHIPPED** | No client field maps to hooks/devices/mounts/scheduling. |
| System-managed default runtime (`RuntimeSpec`, optional `CreateSandbox` fields) | **SHIPPED** | `GET /v1/projects/{project}/runtimes/default`. |
| Timeouts: `startupSeconds`, `maximumRuntimeSeconds`, `idleSeconds` | **SHIPPED** | Startup deadline in the controller; max runtime via Pod `activeDeadlineSeconds`; idle via `sandbox-init` `idleSeconds` + controller (`FINISHED`/`IDLE_TIMEOUT`). Idle enforcement needs a live supervisor probe, so any tick where the probe fails (crash/unreachable/non-200) is skipped and logged; `maximumRuntimeSeconds` is the backstop. |
| Warm-node capacity via balloon Pods | **PARTIAL** | Implemented; `IGNITION_MIN_WARM=0` in every overlay, so no standing warm pool and the 9s p95 API-to-`READY` SLO is unmeasured (`ignition_sandbox_stage_latency_seconds` is already emitted for when a load run happens). |
| `nativeEntrypoint` (run the image's own entrypoint as PID 1) | **PARTIAL** | Works; weaker readiness, no exec/idle-tracking, same security context. `command`/`args` override the image `ENTRYPOINT`/`CMD` Kubernetes-style. |
| Managed `command`/`args` → supervised main process | **SHIPPED** | `CreateSandbox` with `command`/`args` (and `nativeEntrypoint: false`) creates a `Process` row for the argv in the same transaction; `exec`/PTY/idle apply to it. |
| Ephemeral `/scratch` emptyDir | **SHIPPED** | Lost on node loss — part of the public contract. |
| Read-only dataset / artifact mounts, content caches | **PROPOSED** | |
| Writable persistent Volumes, SESSION memory snapshots | **out of scope** | Not on any roadmap. |
| `BARE_METAL` compute environment | **not built** | In the contract; fails closed with `COMPUTE_ENVIRONMENT_UNAVAILABLE`. |

## Exec data plane

| Capability | Status | Notes |
|---|---|---|
| `ignition-api` mints an HS256 exec stream token | **SHIPPED** | Separate audience from access JWTs. |
| `sandbox-init` process supervision (`downwardAPI` desired file, `:8081` observed + stdio) | **SHIPPED** | Controller polls observed state; no Kubernetes credential in the sandbox. |
| `ignition-gateway` — validates the token, resolves the Pod by label, proxies the attach WebSocket | **SHIPPED** | `internal/gateway`. Deployed by every overlay. Resolver readiness mirrors the controller (CPU: kubelet `PodReady`; GPU: also `init-healthy` + canonical GPU UUID) and rejects a token whose `generation` ≠ the Pod's `ignition.io/generation` annotation. |
| Public WebSocket `Ingress` for `ignition-gateway` | **SHIPPED** | `staging`/`prod`: `Ingress` + `ManagedCertificate` (`gateway-ingress.yaml`), backend `timeoutSec: 3600`. `dev`/`anyscale-staging`: no DNS → `kubectl port-forward svc/ignition-gateway 8443:8080`. |
| PTY allocation for exec | **SHIPPED** | `pty: true` (+ optional `ptyRows`/`ptyCols`) allocates a real PTY in `sandbox-init`; output is merged on the stdout channel. Mid-session resize is not wired. |
| Durable exec spool / offset-based reconnect (`ignition-ingress`, route table) | **DEFERRED** | Shipped path uses a small in-memory replay buffer. |

## CLI and SDKs

| Capability | Status | Notes |
|---|---|---|
| `ignitionctl` (`internal/cli`) — login/context, sandbox + process + operation lifecycle, `exec` with streaming | **SHIPPED** | `-o json`, stable exit codes, polling fallback when no gateway. |
| Python `ignition-sandbox` — sync client, no deps | **SHIPPED** | `sdks/python`. Sandbox/process/operation lifecycle, `:watch`, exec streaming (built-in WS client) + polling fallback. `run(cmd, capture=True)` collects stdout/stderr bytes onto `ExecResult`. |
| TypeScript `@ignition/sandbox` — async client, no deps | **SHIPPED** | `sdks/typescript`. Same surface; global `fetch`/`WebSocket` (Node 22+). |
| Native `async` Python client; PTY resize; text wrappers / backpressure helpers | **PROPOSED** | Target contract in [api-contract](ignition-api-contract.md). |

These live in `examples/`, not on the deploy path — they exercise the shipped
public API, they are not platform features.

| Item | Status | Notes |
|---|---|---|
| `examples/agentic-rl/` — sandbox as an RL environment / rollout worker (RLVR) | **built + tested** | Six code-fix tasks with pytest verifiers; in-sandbox agent harness + out-of-cluster rollout controller (topology A: agent in the sandbox, dials out; topology B: driver drives a bare sandbox over the exec stream). Hermetic test suite + CI (`deploy/cloudbuild/pr-examples.yaml`). Design: [agentic-rl-on-ignition](agentic-rl-on-ignition.md); runbook: [agentic-rl-example](../guides/agentic-rl-example.md). |
| `examples/agentic-rl/` — real GRPO step (`swe_mini/trainer/grpo_trl.py`) | **built + tested** | `trl.GRPOTrainer` via its `rollout_func` hook — TRL owns the optimizer + group-relative advantage; the example owns generation (the harness) and reward (the verifier). Verified against `trl==1.13.0` (1-step, CPU, tiny model). A real run needs a GPU + a vLLM endpoint. The trainer / inference are **not** part of Ignition. |

## Images

| Capability | Status | Notes |
|---|---|---|
| Image delivery on GKE | **SHIPPED** | Delegated to GKE image streaming; no Ignition-owned data path. |
| v0 image admission (`POST/GET /v1/projects/{project}/images`) — resolve `sourceRef` to a digest, static streaming-eligibility check | **PARTIAL** | `internal/imagecatalog`. Registry-host allowlist + SSRF guard (no loopback/private/link-local/`169.254.169.254`, post-DNS) + resolve timeout + sanitized client errors are in place (`guard.go`); verified end to end with digest-pinned scheduling on `anyscale-demo`. Still missing: signature/provenance/scan, same-region Ignition-owned copy — see [image-delivery](ignition-image-delivery.md#security-status). |
| Digest-pinned `imageId` | **not built** | Controller resolves a bare path under the Artifact Registry sandbox prefix. |
| Same-region import, signature/provenance verification, scanning, signed catalog | **PROPOSED** | |
| Secondary boot-disk cache cohorts, adaptive lazy/eager selection, access profiles | **PROPOSED** | |
| GKE Pod snapshot orchestration; stratified delivery (`ignition-strata`) | **PROPOSED** | GKE Pod Snapshots are GA; Ignition does not create/qualify/select them. |
| Custom GCE lazy backends (Nydus / eStargz / SOCI); `ignitionfs` | **DEFERRED** / rejected | `ignitionfs` is not approved. |

## Operations and security

| Capability | Status | Notes |
|---|---|---|
| Per-service GCP + KSA identities, least-privilege Cloud SQL roles | **SHIPPED** | |
| Private networking, Cloud SQL HA + PITR | **SHIPPED** | |
| Audit-log lines on RBAC mutations | **SHIPPED** | |
| CI pipeline (PR-merge / nightly / staging), Terraform for cluster + SQL + prober + IAP | **SHIPPED** | `deploy/PIPELINE.md`. |
| Critical-user-journey prober | **SHIPPED** | Runs on staging. |
| SPIFFE/SPIRE internal identity | **PROPOSED** | Google API auth + internal auth are separate mechanisms today. |
| Usage / metering ledger, reconciler | **PROPOSED** | |
| Cross-region DR drills, launch gates, threat-model review | **PROPOSED** | Targets in [production-operations](ignition-production-operations.md). |

## Deferred custom runtime (none built)

`ignition-scheduler`, `ignition-worker-control`, `ignition-fleet`, `ignition-artifacts`,
`ignition-builder`, `ignitiond`, `ignition-hostd`, `snapshotd`, `ignition-ingress`,
`ignition-gpu-health`, the GCE MIG worker fleet, golden startup snapshots, and
CUDA checkpoint/restore. Retained as the design of record in
[deferred-runtime](ignition-deferred-runtime.md); not on the deploy path.
