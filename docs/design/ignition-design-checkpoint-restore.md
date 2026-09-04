# Ignition Checkpoint and Restore Design

**Status:** Not implemented — design of record only for the deferred custom
GCE/MIG worker runtime. Managed GKE Pod snapshots are specified separately in
[Images and Startup Acceleration](ignition-design-images-startup.md#managed-gke-snapshot-path).

> No checkpoint/restore orchestration is built. GKE provides managed Pod
> snapshots, including compatible GPU state, generally available on GKE
> 1.35.3-gke.1234000 or later; Ignition does not yet create, qualify, select,
> or restore them. This document is retained for a possible
> direct custom Compute Engine implementation only; it must not be used to
> reimplement capabilities already provided by managed GKE without a measured
> requirement — see
> [GKE Sandbox — Relationship to the custom runtime](ignition-design-gke-sandbox.md#relationship-to-the-custom-runtime).

**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines golden startup snapshot coordination, the `ignition-hostd` gVisor/CUDA runtime boundary, filesystem and process checkpoint ordering, manifests, encryption, storage, compatibility validation, restore, and failure behavior.

## Snapshot classes

- **Golden startup (initial production):** immutable, request-free filesystem/process/GPU deployment artifact for allowlisted stateless inference.
- **Session (deferred):** proposed client-requested runtime state; it is not a public or supported memory-snapshot class.
- **Recovery (deferred):** proposed periodic runtime state; workers do not create it in initial production.
- **Filesystem-only (deferred):** proposed persistent filesystem state without live process/GPU state.

Golden snapshots are created by `ignition-builder`. Initial production exposes no public session memory-snapshot API and makes no periodic runtime-recovery promise. Spot and host failure recreate from the immutable golden snapshot; in-flight requests may fail.

## Ownership and process boundary

`snapshotd` runs unprivileged on each worker and owns:

- lifecycle-hook and ingress-quiesce coordination;
- package manifest;
- hashing and authenticated encryption;
- GCS upload/download;
- local prefetch;
- compatibility and tenant validation;
- cancellation and partial cleanup.

`ignition-hostd` is the privileged typed runtime broker and the initial sandbox lifecycle owner. It alone invokes direct `runsc create`, `start`, `checkpoint`, `fscheckpoint`, `restore`, and `delete`, and accesses containerd content and snapshot services. `snapshotd` cannot invoke arbitrary commands; it requests typed operations from `ignition-hostd`. `ignitiond` never receives the containerd root socket or direct `runsc` access.

Containerd provides content and lazy snapshot services only. The initial release does not use the containerd task API or `containerd-shim-runsc-v1`; either path requires released upstream support and separate qualification before enablement.

Neither service manually enumerates CUDA PIDs when the validated `runsc --cuda-checkpoint-path` integration owns that operation.

## Local API

The unprivileged orchestration API exposes:

```text
PrepareGoldenSnapshot
RestoreSnapshot
PrefetchSnapshot
DeleteLocalSnapshot
GetOperation
CancelOperation
```

Every request includes sandbox ID, expected compatibility tuple, lease fencing token, deadline, snapshot class, and signed authorization context.

Session and recovery creation methods are intentionally absent. `snapshotd` calls a separate root-owned `ignition-hostd` Unix-domain API containing typed `RunscCheckpoint`, `RunscFSCheckpoint`, and `RunscRestore` operations.

## Runtime broker adapter

```go
type RuntimeBrokerAdapter interface {
    Validate(context.Context) error
    FSCheckpoint(context.Context, FSCheckpointRequest) (FSCheckpointResult, error)
    Checkpoint(context.Context, CheckpointRequest) (CheckpointResult, error)
    Restore(context.Context, RestoreRequest) (RestoreResult, error)
}
```

Pin and record exact `runsc` build, argv, environment, runtime ID, timing, and exit diagnostics. `ignition-hostd` invokes it directly without a shell.

The compatibility tuple also pins the exact `cuda-checkpoint` binary digest and in-container path. The broker injects that binary read-only inside the sandbox and rejects a missing, writable, differently hashed, or image-selected binary.

## Filesystem checkpoint contract

Before process or GPU checkpoint, `ignition-hostd` invokes gVisor `fscheckpoint` for:

- the rootfs writable upper on disk-backed overlay2; and
- every declared disk-backed tmpfs admitted as required startup state.

User-created tmpfs mounts, user-created mounts, and ephemeral scratch are excluded. Allowlisted workloads must not place required startup state there. Build-time probes fail closed if restore depends on excluded state. Filesystem restore completes before process/GPU restore begins.

## Manifest

Include:

- schema and snapshot class;
- snapshot, project, sandbox, image, and startup IDs;
- compatibility tuple and CPU-feature hash, including the pinned `cuda-checkpoint` digest and in-container path;
- sequence number and timestamps;
- lifecycle-contract version;
- one entry for every file under each opaque `runsc` checkpoint and `fscheckpoint` directory, with directory kind, relative path, mode, size, digest, and `required` flag;
- encryption algorithm and KMS key version;
- retention and authorization policy;
- parent reference only when a real incremental format exists.

Authenticate the canonical manifest independently of GCS IAM.

Directory traversal, duplicate normalized paths, absolute paths, unsupported file types, unexpected required files, mode mismatch, size mismatch, digest mismatch, and missing required files fail closed. The package never models only `pages.img`; opaque directories are recursively manifested without assigning semantics to individual runtime files.

## Create sequence

1. validate authorization, lease, tuple, and deadline;
2. remove ingress route and drain active requests;
3. verify the workload is in the stateless golden-snapshot allowlist and has no active requests;
4. run `prepare_snapshot`;
5. validate the disk-backed overlay2 upper and declared disk-backed tmpfs set;
6. invoke gVisor `fscheckpoint` and wait for durable completion;
7. invoke integrated process/GPU checkpoint with the pinned in-sandbox `cuda-checkpoint` binary;
8. determine whether source sandbox remains safe;
9. recursively manifest every filesystem and runtime checkpoint file;
10. hash and envelope-encrypt artifacts;
11. upload to a temporary object prefix;
12. upload authenticated manifest;
13. commit catalog state atomically;
14. terminate or quarantine the build sandbox according to result.

Only full immutable golden startup snapshots are supported in the initial release.

## Restore sequence

1. authorize project and snapshot class;
2. verify manifest signature and schema;
3. verify image, tuple, GPU SKU, CPU features, and lifecycle contract;
4. reserve RAM, disk, and GPU;
5. download and decrypt into a protected local directory;
6. verify every manifested path, mode, size, digest, and required flag;
7. apply the selected qualified I/O strategy to all opaque checkpoint files;
8. restore the rootfs writable upper and declared disk-backed tmpfs from `fscheckpoint`;
9. after filesystem completion, invoke process/GPU runtime restore;
10. wait for any asynchronous gVisor background restore to finish and release all artifact references;
11. run `after_restore`;
12. verify exact GPU visibility and application health;
13. activate ingress;
14. securely remove temporary plaintext.

No compatibility fallback occurs silently. Decrypted files are retained until asynchronous restore completion; early deletion is a correctness failure.

## Cross-host qualification gate

Publication and production enablement require repeated restores onto a different worker with a distinct physical GPU. Qualification covers:

- persistence mode and required `cuInit` ordering before checkpoint and restore;
- source-to-target GPU UUID and PCI identity remapping without ordinal assumptions;
- exact visible-device identity inside the sandbox;
- NVML enumeration and telemetry;
- PyTorch allocation, kernels, synchronization, and numerical checks;
- CUDA graph replay and recapture behavior as required by the workload;
- model-server readiness, inference correctness, concurrency, and lifecycle hooks.

Passing same-host or same-physical-GPU tests is insufficient.

## Encryption

- Generate a unique data-encryption key per snapshot.
- Use authenticated encryption.
- Wrap the key with a project/domain KMS key.
- Keep plaintext only on the authorized worker.
- Separate object-read permission from KMS-decrypt permission.
- Rotate wrapping keys without rewriting snapshot plaintext.

## Interruption behavior

Spot or host failure recreates from the last published immutable golden startup snapshot. No periodic recovery checkpoint is attempted, and in-flight requests may fail. Maintenance cordons and drains when possible, then recreates from the golden snapshot on a qualified worker.

## Failure rules

- Unknown schema, wrong project, stale generation, corrupt hash, wrong tuple, or unavailable key fails closed.
- Partial objects remain unreachable and are garbage-collected.
- Partial plaintext is deleted.
- A CUDA checkpoint error that may leave state inconsistent quarantines the sandbox.
- Cancellation does not skip cleanup.
- Catalog never points to an incomplete package.
- Logical lease release never makes the physical GPU reusable. Reuse requires proof that all `runsc` processes, NVIDIA contexts, mappings, mounts, namespaces, and cgroups are gone, followed by GPU health validation.
- Any ambiguous cleanup crosses the physical release barrier only by quarantining and recreating the VM.

## SLOs

Define by VRAM captured, total bytes, cache state, GPU SKU, and restore I/O strategy. Measure quiesce, filesystem checkpoint, CPU checkpoint, GPU checkpoint, encryption, upload, download, prefetch/background restore, filesystem restore, CPU restore, GPU restore, application wake, and readiness separately.

Benchmark full prefetch against gVisor background restore, including compression disabled, direct I/O, and zero-page exclusion. Budget peak host CPU RSS and GPU VRAM, and measure the lifetime of retained decrypted files.

For the validated L4 workload with at most 8 GiB captured VRAM, scoped p95 application-ready targets are at most 20 seconds with a locally cached golden artifact and at most 120 seconds on the cold lazy-image path. These targets do not generalize to other workloads, GPU SKUs, or capture sizes.

## Acceptance

- Only immutable golden startup snapshots for allowlisted stateless inference are enabled; session and periodic recovery memory APIs are absent.
- Repeated same-tuple cross-host restore passes on distinct physical GPUs, including persistence mode/`cuInit`, UUID/PCI remap, NVML, PyTorch, CUDA graphs, and model-server tests.
- Corruption, wrong tenant, incompatible tuple, and revoked key fail closed.
- Every file in opaque `runsc` checkpoint and `fscheckpoint` directories is manifested and verified by relative path, mode, size, digest, and required flag; packages cannot special-case only `pages.img`.
- `fscheckpoint` captures the disk-backed overlay2 upper and all declared disk-backed tmpfs before process/GPU checkpoint; restore proves the reverse dependency order.
- User-created tmpfs/mounts and ephemeral scratch cannot contain required startup state.
- The exact tuple-pinned `cuda-checkpoint` binary is injected read-only at its declared in-container path.
- Checkpointable allowlist tests prohibit managed-memory/UVM allocations while accepting the validated CDI device set, including `/dev/nvidia-uvm`.
- Full-prefetch and gVisor-background-restore benchmarks cover no compression, direct I/O, zero-page exclusion, CPU RSS, and VRAM.
- Decrypted files remain present until asynchronous restore completion.
- The validated L4 workload with at most 8 GiB captured VRAM meets the scoped p95 20-second local-cache and 120-second cold-lazy-image readiness targets.
- Deadline and cancellation complete within bounded time.
- Source safety after failed checkpoint is deterministic.
- Restore leaves no plaintext or stale GPU lease.
- GPU reuse occurs only after the physical release barrier; ambiguous cleanup recreates the VM.
- Spot and maintenance drills recreate from the golden snapshot and make no runtime-state continuity promise.
