# Ignition Default Runtime Design

**Status:** Current — implemented in `ignition-api` and `ignition-controller`.

**Parent:** [GKE Sandbox](ignition-design-gke-sandbox.md)
**Public API contract:** [Create Sandbox API](ignition-sandbox-create-api.md)

## Purpose

Callers should not have to manage a sandbox template or spell out compute,
placement, timeout, and network settings on every `CreateSandbox`. The **default
runtime** is a system-managed `RuntimeSpec` that fills any field a request
leaves unset. There is no project-level template resource.

## RuntimeSpec

```
RuntimeSpec {
  resources  { cpuMilli, memoryMiB, accelerator { type, count } }
  placement  { region, computeEnvironment }
  timeouts   { startupSeconds, maximumRuntimeSeconds, idleSeconds, terminationGraceSeconds }
  network    { internetAccess }
}
```

On `CreateSandbox` every one of these fields is optional. The request is parsed
into a partial `RuntimeSpec`, merged field-by-field over the default runtime,
validated against the platform caps, and then **snapshotted** onto the sandbox.
Later changes to the default runtime never affect existing sandboxes.

`GET /v1/projects/{project}/runtimes/default` returns the resolved default
runtime so a caller can see what a bare create will produce. Read-only;
permission `runtime.get` (every project role).

## The default runtime itself

- **Built-in fallback** (`store.BuiltinDefaultRuntime`): a CPU-only sandbox —
  `accelerator.type = NONE`, 1000m CPU, 2048 MiB, `internetAccess = DISABLED`,
  `computeEnvironment = STANDARD`, and the standard 120/3600/600/20s timeouts.
- **Operator override**: `IGNITION_DEFAULT_RUNTIME`, a JSON `RuntimeSpec` object
  merged over the built-in, so only the changed fields need to be given. It is
  validated at process startup (caps + enum sets) and checked against
  `IGNITION_ALLOWED_ACCELERATORS`; a bad value fails the process rather than
  silently degrading.

`IGNITION_ALLOWED_ACCELERATORS` defaults to `NONE,NVIDIA_L4` so the built-in CPU
default is usable with no extra configuration.

## Accelerator profiles

`internal/k8s/profile.go` is the source of truth for which accelerator types are
serviceable and how each is scheduled.

| Accelerator | Restricted node pool | Internet node pool | Runtime | Device request | One-per-node | Node taint |
|---|---|---|---|---|---|---|
| `NONE` (CPU) | `cpu-sandbox` | `cpu-sandbox-internet` | gVisor | none | no | `ignition.io/sandbox=true` |
| `NVIDIA_L4` | `gpu-sandbox-l4` | `gpu-sandbox-l4-internet` | gVisor | `nvidia.com/gpu: 1` | yes (hostname anti-affinity) | `ignition.io/gpu-sandbox=true` |

Both classes keep the non-negotiable isolation invariants: `runtimeClassName:
gvisor`, `automountServiceAccountToken: false`, read-only root filesystem,
dropped capabilities, `RuntimeDefault` seccomp, writable `/scratch` emptyDir
only. The controller fails a sandbox `WORKLOAD_NOT_SUPPORTED` (no Pod created)
when the accelerator type has no profile.

`network.internetAccess = DISABLED` schedules onto the restricted node pool.
`network.internetAccess = ENABLED` schedules onto the matching internet node
pool in the same GKE cluster. Internet pools use a separate subnet, Pod range,
node network tag, Cloud NAT scope, and firewall policy; they do not require a
separate GKE cluster.

`sandbox-init` receives `IGNITION_ACCELERATOR`. For `NONE`, `/readyz` succeeds as
soon as the supervisor is up; for `NVIDIA_L4` it still verifies exactly one GPU
identity. Kubelet's resulting `PodReady` advances the public state to `READY`.

## Boundaries

- The `cpu-sandbox` GKE node pool is operator-provisioned (see the
  implementation guide). CPU warm-node capacity is opt-in through
  `IGNITION_MIN_WARM_CPU` / `IGNITION_MAX_WARM_CPU`; both default to zero so an
  upgrade does not silently add node cost. `IGNITION_MIN_WARM` /
  `IGNITION_MAX_WARM` remain the NVIDIA L4 buffer.
- `/ignition/init` must be present in the tenant image; the Pod runs it as the
  entrypoint and does not inject it.
- Only the CPU (`NONE`) and `NVIDIA_L4` profiles exist. TPU and other GPU SKUs
  are out of scope; `internal/k8s/profile.go` is the extension point.
