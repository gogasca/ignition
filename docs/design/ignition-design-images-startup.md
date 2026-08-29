# Ignition Images and Startup Acceleration Design

**Status:** Draft v0.1  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines OCI admission, lazy image delivery, content-addressed caches, startup artifact building, immutable golden startup snapshots, application hooks, strategy selection, and invalidation. Initial production supports golden startup memory snapshots only for allowlisted stateless inference workloads; public session snapshots and periodic runtime recovery snapshots are deferred.

## Components

- `ignition-artifacts`: authoritative metadata catalog.
- `ignition-builder`: image conversion and golden-snapshot workflow.
- eStargz or SOCI snapshotter.
- worker page cache and Local SSD cache.
- optional zonal cache after measurement.
- GCS artifact storage.

Containerd is limited to content and lazy snapshot services. Sandbox lifecycle is performed by direct, typed `runsc` operations owned by `ignition-hostd`; neither the containerd task API nor `containerd-shim-runsc-v1` is used in the initial release.

## Image admission

1. resolve mutable reference to immutable OCI digest;
2. verify registry identity, signature, provenance, and project policy;
3. scan metadata and prohibited configuration;
4. create eStargz image or SOCI index;
5. bind lazy metadata to source digest;
6. publish catalog entry atomically.

Tenant code never receives worker registry credentials.

## Lazy loading

Read order:

```text
Linux page cache
→ worker Local SSD
→ optional zonal cache
→ regional/object storage
```

Container creation blocks on authenticated metadata, not complete image transfer. Measure index lookup, mount, first instruction, bytes fetched, decompression CPU, and cache-tier latency.

## Golden startup snapshot

A golden snapshot:

- is immutable;
- contains no request/session data;
- belongs to one project and image/startup revision;
- is keyed by exact compatibility tuple and GPU SKU;
- is verified by restoring on a second worker before publication;
- accelerates new replicas and provides the clean recreation point after Spot or host failure;
- does not preserve in-flight requests or mutable runtime/session state.

Initial production does not promise runtime-state recovery. On Spot or host failure, the platform recreates the sandbox from its golden snapshot and in-flight requests may fail.

Build key:

```text
project
image digest
startup policy revision
snapshot key
GPU SKU
compatibility tuple
lifecycle contract version
cuda-checkpoint digest and in-container path
```

The compatibility tuple pins the exact `cuda-checkpoint` binary digest and its in-container read-only path. The builder injects that exact binary into the sandbox; it never selects a host or image-provided binary by name.

## Build workflow

1. allocate an isolated compatible worker;
2. cold-start the admitted image;
3. run `prepare_snapshot`;
4. wait for declared readiness;
5. verify zero active requests;
6. run application sleep/offload policy;
7. verify that all required startup filesystem state is within the disk-backed overlay2 writable upper or a declared disk-backed tmpfs;
8. ask `snapshotd` to coordinate a gVisor `fscheckpoint` of the writable rootfs upper and every declared disk-backed tmpfs;
9. after filesystem checkpoint completion, capture process state and, when selected, GPU state;
10. manifest, encrypt, and upload under a temporary prefix;
11. restore the filesystem before process/GPU state on a second worker with a distinct physical GPU;
12. run `after_restore`, compatibility qualification, and functional probes;
13. publish manifest and catalog state;
14. release build workers.

Publishing transitions `BUILDING → READY` only after verification.

The rootfs uses disk-backed overlay2, and every tmpfs declared as snapshot-required is configured with a disk-backed gVisor checkpoint representation. User-created tmpfs mounts, user-created mounts, and ephemeral scratch are excluded from the golden artifact and must not contain required startup state. Admission and build probes fail when an allowlisted workload relies on excluded state.

## Lifecycle contract

Allowlisted workloads implement:

```text
prepare_snapshot
after_restore
health
drain
abort
```

Hooks have strict deadlines and structured results. vLLM/SGLang adapters may use sleep/wake, weight offload, and KV-cache recreation. Transparent checkpointing is not assumed. The checkpointable allowlist prohibits managed-memory/UVM allocations even though the validated CDI device set may expose `/dev/nvidia-uvm`.

## Strategy selection

Benchmark:

1. lazy-image cold start;
2. CPU snapshot plus GPU initialization;
3. CPU + GPU snapshot;
4. sleep/offload plus wake;
5. ready sandbox reuse.

Select per image/startup revision. Snapshotting storage-bound weights may be slower than loading them; imports, JIT compilation, kernels, and CUDA graphs are stronger candidates.

## Restore I/O strategy

Benchmark complete local prefetch against gVisor background restore for each admitted workload. Test compression disabled, direct I/O, and zero-page exclusion as explicit dimensions, and record page access traces. A strategy is admitted only from reproducible end-to-end readiness results; there is no assumption that `pages.img` is the only artifact.

Keep all decrypted checkpoint and filesystem-checkpoint files until asynchronous/background restore has completed and `runsc` no longer references them. Secure deletion occurs only after that completion barrier. Capacity planning and admission budget both host CPU RSS and captured GPU VRAM, in addition to encrypted/decrypted disk bytes.

## Scoped startup targets

For the validated L4 workload with at most 8 GiB of captured VRAM:

- locally cached golden artifact: p95 application-ready time is at most 20 seconds;
- cold lazy-image path: p95 application-ready time is at most 120 seconds.

These are scoped qualification targets, not general SLOs for other GPU SKUs, larger VRAM captures, arbitrary images, cross-region fetches, or unvalidated workloads.

## Invalidation

Create a new golden artifact when image digest, tuple (including `cuda-checkpoint` digest/path), GPU SKU, lifecycle contract, startup key, filesystem layout, or policy changes. Never mutate a ready artifact.

Revoke immediately for security or correctness defects. New placement ignores revoked artifacts; running sandboxes follow the incident policy.

## Custom filesystem decision

Start with eStargz and SOCI. Build `ignitionfs` only when reproducible tests show an unmet latency, throughput, integrity, or cost requirement that cannot be addressed by configuration or caching.

## Observability

Measure image ingest, conversion, metadata size, cache hit ratio, bytes by tier, mount, first instruction, model ready, golden build, verification restore, invalidation, and selected-strategy accuracy.

## Acceptance

- Digest/signature/index tampering fails closed.
- Golden snapshots contain no request-derived data.
- Golden snapshots are available only to allowlisted stateless inference; session and periodic recovery memory snapshot APIs are absent.
- The manifest covers every file in the opaque `runsc` checkpoint and `fscheckpoint` directories, not only `pages.img`.
- Filesystem capture precedes process/GPU capture, and restore recreates the overlay2 upper and declared disk-backed tmpfs before process state.
- Required startup state in user-created tmpfs/mounts or ephemeral scratch fails admission or build verification.
- Publication requires restore on a second worker with a distinct physical GPU and the full cross-host qualification gate.
- The exact pinned `cuda-checkpoint` digest is injected read-only at the tuple's in-container path.
- Managed-memory/UVM allocation tests fail closed for checkpointable allowlisted workloads.
- Full-prefetch and gVisor-background-restore benchmarks cover no compression, direct I/O, and zero-page exclusion; decrypted artifacts survive until async restore completion.
- Resource tests account for CPU RSS and VRAM.
- The validated L4 workload meets p95 ready targets of 20 seconds for a locally cached artifact and 120 seconds for a cold lazy image, with at most 8 GiB captured VRAM.
- Selected strategy beats or intentionally disables snapshot restore.
- Artifact invalidation prevents new restores within the security-response target.
