# Ignition Worker Runtime and GPU Isolation Design

**Status:** Draft v0.1  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines `ignitiond`, the privileged typed runtime broker `ignition-hostd`, containerd content and snapshot services, gVisor, `nvproxy`, OCI/CDI policy, local reconciliation, GPU health, cleanup, and interruption behavior.

## Process boundary

- `ignitiond`: unprivileged coordinator and control-plane client. It never receives the containerd root socket or permission to invoke `runsc`.
- `ignition-hostd`: initial lifecycle owner and privileged typed runtime broker. It alone performs direct `runsc create`, `start`, `checkpoint`, `fscheckpoint`, `restore`, and `delete`, and accesses containerd content and snapshot services.
- `snapshotd`: unprivileged snapshot packaging and policy coordinator with a separate design. It requests typed checkpoint/restore operations from `ignition-hostd`.
- `ignition-ingress`: unprivileged request and exec proxy.
- containerd and `runsc`: pinned third-party components. Containerd supplies content and lazy snapshot services only.
- `ignition-gpu-health`: supervised DCGM/NVIDIA health adapter. In the GKE MVP this role is realized by `ignition-gpu-agent`, a per-node DaemonSet that attests the sandbox's GPU (canonical UUID, ECC/reset health, no residual compute processes) via `nvidia-smi` and gates node reuse; see [GKE Sandbox design](ignition-design-gke-sandbox.md).

Each service uses a dedicated Unix socket, user, filesystem area, and systemd unit.

The initial lifecycle path does not use the containerd task API or `containerd-shim-runsc-v1`. Those paths remain disabled until their required upstream support is released and qualified against Ignition's lifecycle, checkpoint, fencing, and cleanup tests.

## Reconciliation

For desired sandbox creation:

1. validate worker generation, sandbox generation, lease, fencing token, and compatibility tuple;
2. atomically reserve the local GPU;
3. resolve image and optional snapshot metadata;
4. construct server-owned OCI configuration;
5. select the trusted CDI device;
6. create cgroup, namespace, rootfs, scratch, and network;
7. ask `ignition-hostd` to invoke the pinned `runsc` lifecycle operation directly;
8. verify GPU visibility and readiness;
9. activate ingress route;
10. report observed state and timings.

Deletion reverses these steps. Route removal occurs before process termination. A logical lease release does not make the GPU reusable: `ignition-hostd` must prove that `runsc` processes, NVIDIA contexts, device mappings, mounts, namespaces, and cgroups are gone, then pass GPU health validation. Ambiguous cleanup triggers VM quarantine and recreation; it never returns the physical GPU to placement.

## Local state

Persist recoverable journals under `/var/lib/ignition`:

- desired and observed sandbox generation;
- lease and fencing token;
- runtime IDs and containerd content/snapshot references;
- local mount/network/cgroup IDs;
- snapshot operation IDs;
- cleanup progress.

On restart, inspect actual kernel/containerd state before continuing. Never assume the prior step completed.

## Root helper

`ignition-hostd` accepts only typed operations. The API is the sole privileged lifecycle boundary:

```text
CreateSandboxResources
ConfigureGPUDevice
MountRootfs
ConfigureNetwork
RunscCreate
RunscStart
RunscCheckpoint
RunscFSCheckpoint
RunscRestore
RunscDelete
ResolveContainerdContent
MountContainerdSnapshot
DestroySandboxResources
InspectResources
```

It validates identifiers, canonical paths, ownership, resource ceilings, device UUID, lease token, allowed runtime argv, and allowed content/snapshot operations. It cannot access the public network, tenant secrets, arbitrary host paths, or arbitrary commands. Its containerd credential is restricted to required content and snapshot services; task creation and task lifecycle calls are denied.

## OCI policy

Tenant-controlled:

- immutable image digest;
- argv;
- bounded non-secret environment;
- declared work directory;
- CPU/RAM class;
- approved egress policy.

Always deny:

- privileged containers;
- host PID/IPC/user/network namespaces;
- host path mounts;
- arbitrary hooks or CDI records;
- arbitrary devices;
- added Linux capabilities;
- metadata-server access;
- writable rootfs unless policy permits it.

## gVisor

`runsc` is the primary tenant/host syscall boundary:

- userspace sentry;
- gofer/mount integration;
- netstack;
- synthetic proc/sys views;
- `nvproxy`.

Run one mutually untrusted tenant per gVisor sandbox. Initial production places only one hostile tenant on each GPU VM and assigns that tenant the whole GPU; multiple hostile tenants never share the host NVIDIA driver on the same worker. Host networking is unavailable for this tier.

## GPU and CDI

- Discover GPU UUID, PCI identity, SKU, driver, and health.
- Generate CDI records with a pinned `nvidia-ctk` only from trusted boot-time code.
- Make CDI storage writable only by trusted services.
- Disable uncontrolled CDI refresh and reject records that were not generated and admitted by the worker image.
- Validate and hash every trusted CDI hook, mount, environment entry, and device node; fail closed if the admitted record changes.
- Select the whole leased GPU by UUID, not ordinal, and expose its validated CDI device set, including `/dev/nvidia-uvm` when required by the admitted CUDA stack.
- For checkpointable workloads, prohibit managed-memory/UVM allocations by allowlist policy and qualification tests; do not claim that hiding the UVM node enforces this restriction.
- Reject IPC, NCCL, unvalidated capabilities, extra device nodes, and unsupported ioctls.
- Verify device identity from inside the sandbox before readiness.

`nvproxy` reduces direct host-kernel exposure but the host NVIDIA driver remains trusted. Do not claim VM-equivalent GPU isolation.

## GPU health

Boot admission:

1. enumerate expected device;
2. validate tuple;
3. check ECC/retired pages;
4. run allocation and short GEMM;
5. report healthy.

Severe XID or DCGM events remove ingress, cordon the worker, stop placement, terminate affected sandboxes, and quarantine the VM for replacement. Initial production does not attempt a runtime snapshot during GPU failure.

## Interruption

- Spot or host failure: cordon when possible and recreate from the immutable golden startup snapshot. In-flight requests may fail.
- Maintenance: cordon and drain when time permits, then recreate elsewhere from the golden startup snapshot.
- Control-plane outage: local deadline and cordon logic remains operational.

Initial production does not create periodic runtime recovery snapshots and does not offer public session snapshots. Stateful continuation remains deferred until its runtime semantics and cross-host behavior are separately qualified.

## Security tests

Test host filesystem access, ptrace, proc/sys leakage, metadata access, namespace escape, unauthorized nodes, malformed or changed OCI/CDI, wrong-GPU visibility, capability-node leakage, managed-memory/UVM allocation by checkpointable workloads, fork/process bombs, pinned-memory exhaustion, and GPU denial of service.

## Acceptance

- 1,000 create/restore/exec/delete cycles without leaked resources.
- No duplicate or broadened GPU access.
- Restart at every reconciliation step converges safely.
- Stale fencing tokens are rejected.
- Root helper rejects malformed paths, IDs, limits, and devices.
- `ignitiond` cannot open the containerd root socket, invoke `runsc`, or reach containerd task APIs.
- Lifecycle tests prove direct typed `ignition-hostd` ownership and prove that the containerd task API and `containerd-shim-runsc-v1` are unused.
- CDI tests pin `nvidia-ctk`, select by GPU UUID, detect hook/mount/environment/device-node drift, disable uncontrolled refresh, expose the validated UVM node, and reject managed-memory use for checkpointable workloads.
- Placement tests prove one hostile tenant and one whole GPU per GPU VM.
- A GPU is never reassigned before the physical release barrier; every ambiguous cleanup recreates the VM.
- Spot and host-failure drills recreate from a golden snapshot and allow in-flight requests to fail without promising runtime-state recovery.
- All gVisor and GPU negative tests pass.
- GPU health failures quarantine rather than reuse the VM.
