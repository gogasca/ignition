# Ignition status: what is built

One table per area. This is the fast answer to "is X implemented?" — the design
docs carry the detail. The [implementation guide](../guides/ignition-implementation.md)
is authoritative for exactly what is deployed and how.

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
| Google OIDC / Cloud IAP authentication | **SHIPPED** | Verified end to end on staging. IAP rollout needs a public Ingress + Workspace domain. |
| SQL-backed project RBAC (`roleBindings`, last-owner guard, audit line) | **SHIPPED** | |
| Sandbox lifecycle: create / get / list / terminate / watch (SSE) | **SHIPPED** | |
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
| `NVIDIA_L4` GPU sandbox, one whole GPU, one sandbox per node | **PARTIAL** | Code complete; a real L4 sandbox reaching `READY` is not yet exercised (dev L4 quota). |
| `ignition-gpu-agent` — GPU identity + health attestation, node-reuse gating | **SHIPPED** | Privileged DaemonSet on the GPU pool. |
| `sandbox-init` — readiness probe + tenant-process supervision | **SHIPPED** | |
| Server-owned Pod spec (gVisor, read-only root, dropped caps, no SA token) | **SHIPPED** | No client field maps to hooks/devices/mounts/scheduling. |
| System-managed default runtime (`RuntimeSpec`, optional `CreateSandbox` fields) | **SHIPPED** | `GET /v1/projects/{project}/runtimes/default`. |
| Timeouts: `startupSeconds`, `maximumRuntimeSeconds`, `idleSeconds` | **SHIPPED** | Startup deadline in the controller; max runtime via Pod `activeDeadlineSeconds`; idle via `sandbox-init` `idleSeconds` + controller (`FINISHED`/`IDLE_TIMEOUT`). No idle enforcement for `nativeEntrypoint` (no supervisor). |
| Warm-node capacity via balloon Pods | **PARTIAL** | Implemented; dev runs `IGNITION_MIN_WARM=0`, not measured. CPU warm pool opt-in. |
| `nativeEntrypoint` (run the image's own entrypoint as PID 1) | **PARTIAL** | Works; weaker readiness, no exec/idle-tracking, same security context. |
| Ephemeral `/scratch` emptyDir | **SHIPPED** | Lost on node loss — part of the public contract. |
| Read-only dataset / artifact mounts, content caches | **PROPOSED** | |
| Writable persistent Volumes, SESSION memory snapshots | **out of scope** | Not on any roadmap. |
| `BARE_METAL` compute environment | **not built** | In the contract; fails closed with `COMPUTE_ENVIRONMENT_UNAVAILABLE`. |

## Exec data plane

| Capability | Status | Notes |
|---|---|---|
| `ignition-api` mints an HS256 exec stream token | **SHIPPED** | Separate audience from access JWTs. |
| `sandbox-init` process supervision (`downwardAPI` desired file, `:8081` observed + stdio) | **SHIPPED** | Controller polls observed state; no Kubernetes credential in the sandbox. |
| `ignition-gateway` — validates the token, resolves the Pod by label, proxies the attach WebSocket | **PARTIAL** | Built (`internal/gateway`). Deployed only in the `dev` overlay. |
| Public WebSocket Ingress for `ignition-gateway` | **not built** | Non-`dev` overlays also need an image mapping + `IGNITION_GATEWAY_URL`. |
| PTY allocation for exec | **not built** | Accepted in the contract, not honored. |
| Durable exec spool / offset-based reconnect (`ignition-ingress`, route table) | **DEFERRED** | Shipped path uses a small in-memory replay buffer. |

## CLI and SDKs

| Capability | Status | Notes |
|---|---|---|
| `ignitionctl` (`internal/cli`) — login/context, sandbox + process + operation lifecycle, `exec` with streaming | **SHIPPED** | `-o json`, stable exit codes, polling fallback when no gateway. |
| Python `ignition-sandbox` — sync/async, bounded batch | **SHIPPED** | Control-plane lifecycle. |
| TypeScript `@ignition/sandbox` — bounded batch | **SHIPPED** | Control-plane lifecycle. |
| Richer SDK streaming (text wrappers, backpressure, reconnect credentials) | **PROPOSED** | Target contract in [api-contract](ignition-api-contract.md). |

## Images

| Capability | Status | Notes |
|---|---|---|
| Image delivery on GKE | **SHIPPED** | Delegated to GKE image streaming; no Ignition-owned data path. |
| v0 image admission (`POST/GET /v1/projects/{project}/images`) — resolve `sourceRef` to a digest, static streaming-eligibility check | **PARTIAL** | `internal/imagecatalog`. **Security gap:** no registry-host allowlist, no SSRF guard, no signature/provenance/scan, no same-region copy — see [image-delivery](ignition-image-delivery.md#security-status). Verified against a real registry, not yet a live GKE cluster. |
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
