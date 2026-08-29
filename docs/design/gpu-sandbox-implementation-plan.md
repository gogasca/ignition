# Ignition GPU Sandbox Implementation Plan

**Status:** Draft v0.4 — reconciled initial-production plan  
**Date:** 2026-08-26  
**Target:** hostile multi-tenant, allowlisted single-GPU CUDA inference on GCP  
**Detailed design:** [Ignition Technical Design](ignition-technical-design.md)  
**Recommended MVP:** [GKE Sandbox MVP](ignition-design-gke-sandbox.md)  
**Build and deploy the API:** [Implementation guide](../guides/ignition-implementation.md)  
**Supersession:** this production plan supersedes the earlier monolithic/POC roadmap.

## 1. Executive decision

Ignition is feasible for a constrained initial production release only if runtime/GPU compatibility, isolation, golden restore, physical cleanup, and production operations pass explicit gates.

Core decisions:

- Run GPU workers as immutable GCE VMs and the CPU control plane as independent services on regional GKE.
- Use gVisor `runsc` with `nvproxy`; keep Firecracker GPU passthrough off the production critical path.
- Place exactly one hostile customer workload on each GPU VM and lease its one whole GPU to one active sandbox.
- Make `ignition-hostd` the privileged typed runtime broker and sole direct owner of the `runsc` lifecycle.
- Keep `ignitiond`, `snapshotd`, and `ignition-ingress` unprivileged.
- Use containerd only for content and lazy snapshot services. Do not use its task API or `containerd-shim-runsc-v1` for initial lifecycle.
- Pin host, driver, GPU, `runsc`, containerd content/snapshot behavior, CUDA userspace, workload, CPU policy, and injected `cuda-checkpoint` digest/path as a compatibility tuple.
- Support only immutable golden startup snapshots initially. Do not expose public Session/runtime recovery snapshots.
- Treat scratch as ephemeral and dataset/artifact mounts as read-only. Do not expose writable persistent Volumes.
- Recreate from golden startup state after Spot, host loss, or maintenance; retry or fail in-flight work rather than promising transparent continuation.

The canonical customer hierarchy is `organization → project`. Organization is the billing/policy boundary and project is the resource, quota, authorization, idempotency, artifact ownership, and fairness boundary. “Tenant” remains only the security term for mutually untrusted code.

## 2. Product and threat contract

### 2.1 Supported

- Linux amd64.
- One NVIDIA GPU and one hostile tenant per GPU VM.
- CUDA compute/utility capabilities for explicitly qualified model-server, framework, model, and version combinations.
- Server-owned OCI/CDI configuration and default-deny networking.
- Authenticated exec.
- Lazy immutable images, ephemeral scratch, and read-only project-authorized datasets.
- Application lifecycle hooks for golden snapshot preparation and restore.

### 2.2 Excluded

- Multi-GPU, NCCL, training, MIG sharing, and hostile-tenant time sharing.
- Writable persistent Volumes.
- Public Session, filesystem-only, or runtime recovery snapshots.
- Periodic recovery memory snapshots and live GPU migration.
- Unvalidated ioctls, arbitrary OCI hooks/CDI, host networking, and arbitrary devices.
- Firecracker GPU passthrough, custom ImageFS, raw public TCP, generic public HTTP/2, and end-to-end gRPC ports.

### 2.3 Availability

- Warm-pool exhaustion queues to a database-time deadline and then returns retryable `CAPACITY_UNAVAILABLE`.
- Spot and host loss recreate from the last published golden startup artifact. In-flight requests may fail.
- Maintenance drains when time permits, then recreates from golden.
- Durability and correctness never depend on GCP extended Spot-notice preview behavior.
- Restore occurs only on a certified compatible worker.

## 3. Production component ownership

### 3.1 GKE services

1. `ignition-api` — Auth0-compatible OIDC/OAuth validation, project authorization, admission, idempotency, operations, and events.
2. `ignition-scheduler` — persistent weighted project fairness, placement, GPU leases, fencing, and desired assignment.
3. `ignition-worker-control` — SPIFFE mTLS worker streams, owner epochs, commands, acknowledgements, and observations.
4. `ignition-gateway` — exec data plane.
5. `ignition-fleet` — warm pools, immutable templates, deterministic MIG scale-in, rollout, and VM recreation.
6. `ignition-artifacts` — image and golden-artifact metadata.
7. `ignition-builder` — image conversion and golden startup snapshot workflow.

Each is an independently deployed production service with a distinct SPIFFE identity, GCP service account where required, database role, resource budget, and autoscaling policy.

### 3.2 Worker services

1. `ignitiond` — unprivileged desired-state coordinator; no direct `runsc`, containerd root socket, task API, or shim access.
2. `ignition-hostd` — privileged typed broker; sole direct owner of `runsc create/start/checkpoint/fscheckpoint/restore/delete` and required containerd content/snapshot access.
3. `snapshotd` — unprivileged golden package, policy, encryption, and transfer coordinator.
4. `ignition-ingress` — unprivileged route and exec-spool proxy.
5. `ignition-gpu-health` — supervised DCGM/NVIDIA health adapter.

The host broker exposes only typed, validated operations and cannot execute arbitrary commands, contact the public network, or read customer secrets.

## 4. Canonical contracts

Detailed schemas and acceptance tests remain authoritative in the [technical design module index](ignition-technical-design.md#module-design-index). This plan tracks implementation and gates rather than duplicating those schemas.

### 4.1 Identity and API

- All customer-owned records use non-null `project_id`; tokens and routes use `projectId`.
- Initial public resources are Project, Image, Secret, Sandbox, Process, Operation, and Event.
- Human login uses Authorization Code with PKCE or Device Grant.
- Service accounts use OAuth Client Credentials with `private_key_jwt`.
- API keys are one-time bootstrap/exchange credentials, not API credentials.
- Idempotency is scoped to principal, organization, project where present, method, and canonical route.
- Public schemas contain no writable Volume or public Snapshot resource.

Public sandbox mapping:

- internal Queued/admission → `CREATING`;
- internal Scheduled → `SCHEDULED`;
- internal Creating, Restoring, Initializing, and Verifying → `STARTED`;
- only internal Ready → `READY`;
- internal Draining → `TERMINATING`;
- internal Stopped → `FINISHED`;
- terminal failure → `FAILED`.

### 4.2 Golden artifact

The canonical golden startup identity is:

```text
project + image digest + startup policy revision
```

Compatibility also binds snapshot key, GPU SKU, tuple, lifecycle contract, filesystem layout, and injected `cuda-checkpoint` digest/read-only in-container path.

Golden creation checkpoints the disk-backed overlay2 writable upper and every declared disk-backed tmpfs with `fscheckpoint` before process/GPU checkpoint. User-created tmpfs, user-created mounts, and ephemeral scratch are excluded and cannot contain required startup state.

The package recursively manifests every file in opaque `runsc` checkpoint and `fscheckpoint` directories by directory kind, normalized relative path, mode, size, digest, and required flag. No implementation treats `pages.img` as the complete normative package. Restore recreates filesystem state before process/GPU state and retains all decrypted files until background restore releases them.

The admitted CDI record is generated by pinned trusted tooling and hashes every hook, mount, environment entry, and device node. It may expose validated `/dev/nvidia-uvm`; checkpointable-workload qualification prohibits managed-memory/UVM allocations rather than claiming node hiding enforces the rule.

### 4.3 Physical GPU release

A database lease release alone cannot make a GPU reusable. Current owner epoch and fencing token must acknowledge process exit, NVIDIA context/device-FD closure, device-map/mount/namespace/cgroup/scratch cleanup, local allocation removal, and GPU/worker health.

Any missing acknowledgement, disconnect, stale token/epoch, state disagreement, or ambiguous cleanup marks the worker `SUSPECT`. Fleet recreates the VM; only clean replacement registration and burn-in return the GPU to placement.

### 4.4 Durable control and data plane

- Cloud SQL PostgreSQL regional HA is authoritative.
- Admission writes idempotency, sandbox, operation, quota reservation, queue row, lifecycle event, and outbox event atomically.
- Worker-control ownership uses durable monotonic owner epochs.
- Outbox dispatch uses database-time claims, broker acknowledgement, bounded retries, DLQ history, audited replay of immutable event IDs, and consumer-side transactional deduplication.
- Gateway routes are Postgres-authoritative and generation-scoped.
- Ingress owns bounded Local-SSD exec spools with byte offsets, cumulative ACKs, stream epochs, and explicit `TRUNCATED`/`GAP`.
- Unknown stdin acknowledgement is not auto-replayed. Reconnect is supported for 10 minutes after process exit.
- Worker artifact reads use exact-path/method, short-lived operation credentials.

## 5. Implementation milestones

### Milestone 0 — Freeze specifications

Deliver:

- authoritative module contracts for identity, ownership, service boundaries, public state, compatibility tuple, golden package, and physical release;
- workload allowlist and test matrix;
- reproducible definitions for queue, startup, application-ready, restore, first token, work loss, and capacity exhaustion.

Exit gate:

- module review has no ownership/schema conflict;
- no production behavior depends on a monolith, function-version model, writable Volume, public Session snapshot, runtime recovery snapshot, or Spot preview notice.

### Milestone 1 — Runtime and checkpoint qualification

Use a G2/L4 worker to:

1. run `nvidia-smi`, vector-add, GEMM, framework, and target-model probes under `runsc --nvproxy`;
2. validate direct typed `ignition-hostd` lifecycle without containerd task API or shim;
3. inject and verify the tuple-pinned `cuda-checkpoint` binary read-only inside the sandbox;
4. checkpoint disk-backed overlay2/declaration-backed tmpfs before process/GPU state;
5. recursively manifest opaque checkpoint directories;
6. restore cross-host onto a distinct physical GPU;
7. validate persistence mode/`cuInit`, UUID/PCI remap, NVML, PyTorch, CUDA graphs, hooks, and inference correctness;
8. compare local golden restore, cold lazy image, CPU-only, and CPU+GPU strategies.

Hard NO-GO:

- unsupported required CUDA behavior or unqualified patch dependency;
- nondeterministic cross-host restore;
- required managed-memory/UVM, IPC, NCCL, or unsupported ioctl behavior;
- incomplete filesystem capture or reliance on excluded scratch/user-created mounts;
- any isolation or wrong-GPU visibility failure.

### Milestone 2 — Secure worker runtime

Build `ignitiond`, `ignition-hostd`, `snapshotd`, `ignition-ingress`, and `ignition-gpu-health`; integrate containerd content/lazy snapshots, trusted OCI/CDI, cgroups, namespaces, rootfs, scratch, default-deny egress, journals, interruption handling, and physical cleanup acknowledgements.

Exit gate:

- 1,000 create/restore/exec/delete cycles leak no resources;
- restart at every reconciliation step converges safely;
- stale fencing/owner epochs fail;
- one hostile tenant and one whole GPU per VM is enforced;
- every ambiguous cleanup recreates the VM.

### Milestone 3 — Durable production control plane

Deploy independent API, scheduler, worker-control, gateway, fleet, artifact, and builder services on regional three-zone GKE. Implement Cloud SQL HA and least-privilege roles; admission, queue fairness, leases, owner epochs, routes, operations, outbox/DLQ/dedup, usage ledger, SPIRE, and GKE Workload Identity Federation.

Exit gate:

- duplicate-request, concurrent lease, stale-command, stream failover, outbox crash/replay, zone-loss, and Cloud SQL failover tests pass;
- database grants prevent cross-owner writes;
- no static service-account keys exist.

### Milestone 4 — Client, gateway, and storage boundary

Build REST/gRPC definitions, Python/TypeScript SDKs, `ignitionctl`, exec attachment/reconnect, and egress proxy enforcement.

Exit gate:

- one black-box suite passes REST, SDK, and CLI;
- Volume and Session snapshot schemas/routes/handles are absent;
- binary framing, offsets, ACK, EOF/EXIT, detach, PTY, truncation/gap, and unknown-stdin-ACK tests pass;
- dataset mounts remain read-only and scratch is lost on worker failure.

### Milestone 5 — Images, golden startup, and fleet

Select eStargz or SOCI, implement builder/catalog atomic publication, bake immutable workers, implement warm pools, deterministic instance-specific scale-in, blue/green tuple rollout, and artifact invalidation.

Exit gate:

- second-worker restore on a distinct physical GPU passes;
- all opaque files are manifested and verified;
- cache-loss correctness passes;
- exact-worker scale-in races cannot delete a substitute;
- tuple rollout and rollback maintain compatible capacity.

### Milestone 6 — Production launch

Run:

- adversarial gVisor/GPU isolation and credential-boundary tests;
- GKE zone loss, Cloud SQL zonal failover, regional restore, Pub/Sub redelivery, ingress/gateway replacement, worker loss, Spot, and maintenance drills;
- backup, PITR, KMS/signing-key rotation, SBOM/provenance/scanning, sustained/burst load, metering reconciliation, and cost tests;
- canary rollout and seven-day soak.

Exit gate:

- all module acceptance tests pass;
- dashboards, alerts, error-budget policy, runbooks, on-call ownership, rollback, and GO/NO-GO report are complete;
- no known isolation defect remains.

## 6. Production build inventory

First-party deliverables:

1. `ignition-api`
2. `ignition-scheduler`
3. `ignition-worker-control`
4. `ignition-gateway`
5. `ignition-fleet`
6. `ignition-artifacts`
7. `ignition-builder`
8. `ignitiond`
9. `ignition-hostd`
10. `snapshotd`
11. `ignition-ingress`
12. `ignition-gpu-health`
13. `ignitionctl`
14. Python and TypeScript SDKs
15. database migrations, outbox dispatchers, metering/reconciliation, and API descriptors
16. immutable worker image, regional GKE/Cloud SQL/SPIRE/MIG deployment automation
17. conformance, compatibility, isolation, failure-injection, performance, and DR harnesses

Pinned third-party components:

- `runsc`;
- containerd content and snapshot services;
- eStargz or SOCI snapshotter;
- NVIDIA driver, tuple-pinned `cuda-checkpoint`, and DCGM;
- SPIRE and Cloud SQL connector;
- OpenTelemetry collector.

`containerd-shim-runsc-v1` is not in the normative initial-production inventory.

## 7. Initial launch targets

These are initial-production launch targets, not historical guarantees:

- public API availability: **99.9% monthly**;
- gateway request availability: **99.9% monthly**;
- scheduler decision latency after queue claim: **p95 ≤ 100 ms** at launch load;
- successful exec attach to a ready sandbox: **p95 ≤ 1 second**;
- validated L4 workload with **≤ 8 GiB captured VRAM**, locally cached golden artifact: **p95 application-ready ≤ 20 seconds**;
- validated L4 cold lazy-image path: **p95 application-ready ≤ 120 seconds**;
- Cloud SQL zonal failover: **RPO 0, RTO 5 minutes**;
- region disaster recovery: **RPO 15 minutes, RTO 4 hours**.

## 8. Remaining decisions

Resolved: Auth0-compatible OIDC, service-account OAuth, organization/project hierarchy, production service split, runtime ownership, writable-Volume exclusion, public-Session exclusion, runtime-recovery exclusion, and Spot-preview independence.

Still to decide through versioned policy or measurement:

- exact allowlisted model servers, models, CUDA APIs, and versions;
- initial OCI registry;
- eStargz versus SOCI;
- queue deadline and interrupted-request retry policy;
- maximum qualified VRAM/artifact size beyond the launch class;
- golden retention/invalidation policy;
- regions, zones, GPU SKUs, warm-buffer budgets, and stock fallbacks;
- maximum retained exec output within the fixed reconnect protocol.

No remaining decision can weaken project scoping, one-hostile-tenant-per-VM isolation, the physical release barrier, immutable golden-only recovery, or a production gate.
