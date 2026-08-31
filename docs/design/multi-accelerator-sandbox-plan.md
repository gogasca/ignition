# Multi-Accelerator Sandbox Plan (GPU + CPU)

**Status:** Draft v0.1 — implementation plan
**Date:** 2026-08-31
**Parent:** [GKE Sandbox MVP](ignition-design-gke-sandbox.md)
**Public API contract:** [Create Sandbox API](ignition-sandbox-create-api.md)
**Software design:** [API and Controller proposal](ignition-design-api-controller.md)

## Purpose

Today every sandbox is an NVIDIA L4 GPU sandbox. The assumption is hard-coded at
every layer: the API requires `gpu.type` and `gpu.count == 1`, the store models a
single `GPUSpec`, the controller maps exactly one GPU type to exactly one node
pool, and the Pod spec always requests `nvidia.com/gpu: "1"` on the `gvisor`
runtime class.

This plan widens the sandbox to two accelerator classes — **GPU** (`NVIDIA_L4`
initially) and **CPU-only** (`NONE`) — behind one generalized `accelerator`
contract, without changing the isolation model. Both classes run under gVisor.

TPU is explicitly **out of scope** for this plan. GKE Sandbox does not support
TPUs, so a TPU sandbox would run `runc` and collapse the isolation model to the
VM boundary; that is a separate decision. The abstraction introduced here leaves
room for a third accelerator class but ships no TPU code, node pool, or docs.

## Decisions

1. **One `sandbox-init` image**, not one per accelerator. `/ignition/init` is a
   static, accelerator-agnostic Go supervisor. It links no CUDA and no vendor
   runtime; the NVIDIA userspace is injected at runtime by `nvproxy` and the GKE
   device plugin, and a CPU sandbox needs none of it. The readiness probe
   branches at runtime on a controller-set `IGNITION_ACCELERATOR` env var.
2. **No new control-plane binary or container.** One `ignition-controller`
   serves every pool. Accelerator differences live in an `internal/k8s` profile
   table, not a new deployable.
3. **`/ignition/init` is injected via an initContainer** that copies the binary
   onto a shared read-only `emptyDir`. Today the Pod runs the tenant image with
   `command: ["/ignition/init"]` and simply assumes the binary is present; an
   arbitrary tenant image therefore cannot start. Injection fixes that and keeps
   the runtime identical across accelerator classes.
4. **Multiplexing is at node pools and Pod profiles, never at container images.**
5. **Both GPU and CPU sandboxes run `runtimeClassName: gvisor`.** No
   isolation-model fork — that was the reason TPU is excluded.

## Open decisions

- **CPU sandbox packing.** One tenant per node (as with GPU) or bin-pack several
  CPU sandboxes per node under gVisor. Recommendation: bin-pack. There is no
  device to leak; `automountServiceAccountToken: false`, read-only root,
  dropped capabilities, seccomp, and the gVisor boundary still hold. Hostname
  anti-affinity is dropped for the CPU profile only.
- **API field.** Add `resources.accelerator` and keep `resources.gpu` as a
  deprecated alias (recommendation), versus a hard cutover. The alias keeps every
  existing client and test working through the transition.

## Contract

```proto
message ResourceSpec {
  int32 cpu_milli = 1;
  int32 memory_mib = 2;
  GpuSpec gpu = 3 [deprecated = true]; // alias for accelerator
  AcceleratorSpec accelerator = 4;
}

message AcceleratorSpec {
  AcceleratorType type = 1;
  int32 count = 2; // NONE: 0; NVIDIA_L4: 1
}

enum AcceleratorType {
  ACCELERATOR_TYPE_UNSPECIFIED = 0;
  ACCELERATOR_TYPE_NONE = 1;       // CPU-only sandbox
  ACCELERATOR_TYPE_NVIDIA_L4 = 2;
}
```

Public JSON drops the prefix: `ACCELERATOR_TYPE_NVIDIA_L4` -> `"NVIDIA_L4"`,
`ACCELERATOR_TYPE_NONE` -> `"NONE"`.

Resolution and validation in `ignition-api`:

- If `resources.accelerator.type` is set, it wins.
- Else if `resources.gpu.type` is set, `accelerator = {gpu.type, gpu.count}`.
- Else the request is rejected: one of the two is required (there is no implicit
  CPU sandbox; `"NONE"` must be explicit).
- `NONE` requires `count == 0`; `NVIDIA_L4` requires `count == 1`.
- Platform allowlist `IGNITION_ALLOWED_ACCELERATORS` (alias
  `IGNITION_ALLOWED_GPU_TYPES`); default `NVIDIA_L4` only, so CPU sandboxes are
  opt-in per environment until Phase 1 lands.

`resources` is persisted whole as JSONB, so **no database migration is needed**
for the new field. Old rows deserialize with a zero `Accelerator`; the store
normalizes `gpu` -> `accelerator` on read.

## Accelerator profile

`internal/k8s/profile.go` (new):

```go
type Profile struct {
    Accelerator           string // "NONE", "NVIDIA_L4"
    NodePoolValue         string // ignition.io/node-pool label value
    Taint                 string // toleration key; "" for none
    ResourceName          string // "nvidia.com/gpu"; "" for CPU
    PerPodQuantity        string // "1"; "" for CPU
    RequiresAcceleratorID bool   // READY needs a detected device UUID
    HostnameAntiAffinity  bool   // one sandbox per node
    WarmMin, WarmMax      int
}
```

| Profile | Node pool | Runtime | Resource | READY gate | Anti-affinity |
|---|---|---|---|---|---|
| `NVIDIA_L4` | `gpu-sandbox-l4` | gvisor | `nvidia.com/gpu: 1` | init healthy + GPU UUID | yes |
| `NONE` | `cpu-sandbox` | gvisor | none | kubelet `PodReady` | no (pending decision) |

The registry is the single source of truth for which accelerator types are
serviceable. `reconcileSandbox` looks up the profile; a miss (or a type absent
from the allowlist) fails the sandbox with `WORKLOAD_NOT_SUPPORTED`, the same
fail-closed pattern already used for `BARE_METAL`.

## Phased plan

Each phase leaves `main` shippable.

### Phase 0 — Contract and validation

- `sandbox.proto`, `v1.yaml`: `AcceleratorSpec` / `AcceleratorType`; `gpu`
  marked deprecated.
- `internal/store`: `AcceleratorSpec` type, `ResourceSpec.Accelerator`,
  `ValidAccelerator`, `NormalizeAccelerator` (gpu alias -> accelerator, keeps
  `gpu` mirrored for GPU types so the Phase-0 controller is unaffected).
- `internal/api/sandbox.go`, `limits.go`: resolution order above; per-type count
  rules; `IGNITION_ALLOWED_ACCELERATORS`.
- `internal/config/config.go`: `AllowedAccelerators`, `AcceleratorAllowed`,
  env alias.
- **Controller guard:** `reconcileSandbox` fails any non-GPU accelerator with
  `WORKLOAD_NOT_SUPPORTED` until Phase 1. CPU requests are accepted by the API,
  persisted, and then fail closed — no half-built Pod.
- Docs: create-api, gke-sandbox, api-controller.
- Tests: `gpu` alias still valid; `accelerator: {type: NONE}` accepted by the
  API and then fails `WORKLOAD_NOT_SUPPORTED` at reconcile; bad counts rejected.

### Phase 1 — Profile in the controller

- `internal/k8s/profile.go`: `Profile` + registry.
- `internal/k8s/spec.go`: `SandboxPod(sb, imageRef, profile)` — node selector,
  toleration, resource request, anti-affinity from the profile. Add the
  `/ignition/init` initContainer + shared `emptyDir`. Set `IGNITION_ACCELERATOR`.
- `internal/k8s/types.go`, `convert.go`: `Container.GPU string` +
  `GPUResource` const -> `Container.Accelerator{ResourceName, Quantity}`;
  `AnnotGPUUUID` -> `AnnotAcceleratorID`.
- `internal/controller/reconcile.go`: profile lookup replaces
  `NodePoolForGPUType`; CPU pods reach `READY` on `PodReady` alone; Pod
  `DeadlineExceeded` -> new reason `RUNTIME_LIMIT_EXCEEDED` (currently
  mis-reported as `WORKER_LOST`).
- `internal/sandboxinit/init.go`: `detectAssignedAccelerator(kind)` — keep the
  NVIDIA path; CPU is a no-op (`/readyz` == supervisor up).
- Remove the Phase-0 controller guard. Add `NONE` to the default allowlist.

### Phase 2 — Warm pools per accelerator

- `internal/controller/balloons.go`, `internal/capacity`: warm buffer keyed by
  pool; `BalloonPod(profile)`; per-pool min/max. CPU pool default warm 0.
- `internal/k8s/cluster.go`: `ListGPUPool()` -> `ListSandboxPool(label)`;
  cordon check accepts any registered `ignition.io/node-pool` value.
- `internal/controller/reconcile.go`: `pinSandboxNodes` iterates pools.

### Phase 3 — Image handling and the sandbox-init image

- `images/sandbox-init/Dockerfile`: multi-stage rewrite — pinned digests,
  `go.sum` + build cache mounts, `-trimpath -ldflags=-s -w`, non-root, OCI
  labels. One image.
- Controller / `config.go`: digest-pin the resolved image reference
  (`.../sandboxes/{imageId}@sha256:...`) instead of a bare path; reject an
  unresolved reference. Admission / existence hook (stub -> `IMAGE_UNAVAILABLE`).

### Phase 4 — Data plane (independent track)

`ignition-gateway` is a stub and `sandbox-init` has no process supervision, so
exec today returns a token but runs nothing. This track is required regardless
of accelerators.

- `internal/sandboxinit/`: process supervisor on port 8081 — spawn, signal,
  cancel, PTY, output ring buffer, `process-observed` reporting.
- Process delivery: `ignition-gateway` (which can reach the Pod IP) pushes
  desired process specs to `sandbox-init` over the private port, replacing the
  `ignition.io/process-desired` annotation that the credential-free sandbox
  cannot read.
- `internal/gateway/server.go`: verify the stream token (audience + generation
  fencing), proxy WebSocket stdin/stdout, support the reconnect window.
- `deploy/docker/ignition-gateway.Dockerfile` (new) + k8s Deployment.

### Phase 5 — Deploy and infra

- `docs/guides/ignition-implementation.md`: `gcloud container node-pools create
  cpu-sandbox` — gVisor, no accelerator,
  `--node-labels=ignition.io/node-pool=cpu-sandbox`,
  `--node-taints=ignition.io/sandbox=true:NoSchedule`.
- `deploy/k8s`: kustomize component for the CPU pool; priority classes; RBAC
  unchanged (still Pods in one namespace, get/list/patch Nodes).

### Phase 6 — Tests

- `internal/k8s/spec_test.go`: profile -> Pod for `NONE` and `NVIDIA_L4`;
  initContainer injection asserted.
- `internal/controller/reconcile_test.go`: CPU -> `READY` with no accelerator
  annotation; unknown / not-allowlisted -> `WORKLOAD_NOT_SUPPORTED`;
  `DeadlineExceeded` -> `RUNTIME_LIMIT_EXCEEDED`.
- `internal/api/http_contract_test.go`: `accelerator: {type: NONE}` body; `gpu`
  alias still works.
- Data plane: sandbox-init supervisor and gateway token/proxy tests.

## Sequencing

Phases 0–3 are the GPU + CPU core and do not depend on the data plane; ship
them first. Phase 4 is a parallel track. Phase 0 is backward compatible on its
own — CPU requests are accepted but fail closed until Phase 1 removes the guard.

## Acceptance

1. An `accelerator: {type: NVIDIA_L4, count: 1}` request behaves exactly as a
   `gpu: {type: NVIDIA_L4, count: 1}` request does today.
2. A `gpu` request with no `accelerator` still works unchanged.
3. A CPU sandbox (`accelerator: {type: NONE}`) reaches `READY` on a
   `cpu-sandbox` node with no accelerator resource, no GPU annotation, and the
   `gvisor` runtime class.
4. An unsupported accelerator type fails with `WORKLOAD_NOT_SUPPORTED` and
   creates no Pod.
5. Every sandbox Pod carries `/ignition/init` from the injected initContainer,
   regardless of the tenant image contents.
6. One `sandbox-init` image is built and referenced by both profiles.
