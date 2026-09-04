# Ignition Image Data Layer Design

**Status:** Proposed — backend-neutral image admission and delivery are not
implemented. The previously proposed custom FUSE filesystem (`ignitionfs`) is
not approved for implementation.

> The shipped system delegates image delivery to GKE image streaming. This
> document defines the next implementation for arbitrary OCI containers on GKE
> and, if the custom Compute Engine runtime is justified, on GCE. It deliberately
> does not assume a workload language, framework, entrypoint, readiness protocol,
> or file-access pattern.

**Parent:** [Ignition Technical Design](ignition-technical-design.md)
**Sibling:** [Images and Startup Acceleration](ignition-design-images-startup.md)
**Runtime boundary:** [Worker Runtime and GPU Isolation](ignition-design-worker-runtime.md)

## Decision

Ignition does not build a new filesystem first.

- On GKE, use same-region Artifact Registry, GKE image streaming, and versioned
  secondary boot-disk container-image caches for selected node pools.
- On the deferred custom GCE runtime, qualify Nydus RAFS v6/EROFS, eStargz, and
  SOCI, then select the best supported backend per image class. Eager OCI pull is
  always a correctness fallback.
- Ignition owns admission, immutable catalog metadata, optional access profiles,
  cache placement policy, strategy selection, observability, and revocation.
- A custom `ignitionfs` may be reconsidered only after a production-shaped
  benchmark demonstrates a required capability unavailable in the qualified
  upstream backends. General claims about chunking, deduplication, prefetch, or
  partial file reads do not meet that gate because existing backends provide
  those capabilities.

This replaces the earlier design that proposed a flattened, content-addressed
FUSE filesystem. Nydus already provides a merged filesystem tree, chunk-addressed
data, integrity checking, prefetch, local caching, and FUSE or in-kernel EROFS
operation. eStargz also addresses regular files in independently verifiable
chunks, and SOCI addresses compressed layers in configurable spans. Neither may
be described as necessarily downloading an entire file on first read.

Modal's published architecture validates the broad pattern: load a small
filesystem index, fetch content on demand, and serve it through tiered
content-addressed caches. It does not establish that Modal's private filesystem
can be deployed or operated on GKE. The Ignition-specific contribution is instead
a backend-neutral control plane that binds one canonical OCI identity to verified
delivery representations, partitions caches by security domain, and selects lazy,
cached, or eager delivery from measured behavior. That capability is useful even
when GKE owns the data path.

## Goals

1. Start any admitted OCI image without requiring the complete image to be
   downloaded and unpacked first when a lazy backend is selected.
2. Preserve OCI filesystem and process semantics, including the source process
   remaining PID 1. Optimization must not require an access profile, injected
   binary, or application integration.
3. Minimize platform-controlled time to authenticated rootfs availability and
   container start.
4. Select eager delivery when lazy faults would be slower, rather than assuming
   lazy delivery always wins.
5. Isolate private image data and cache observations between projects.
6. Fail closed on identity or integrity errors and fail predictably on backend
   loss.

## Non-goals

- Ignition cannot bound arbitrary application initialization. A container may
  intentionally sleep, compute indefinitely, or read its entire image.
- This layer does not checkpoint process or device memory.
- This layer does not deliver persistent volumes or application datasets.
- Cross-project deduplication of private data is not a goal.
- Transparent recovery of a running sandbox after its rootfs service fails is
  not promised.

## Startup definitions

The following measurements are distinct:

- `image_resolved`: the immutable OCI digest and admitted catalog entry exist;
- `rootfs_ready`: authenticated filesystem metadata is mounted or the eager
  rootfs is unpacked;
- `container_created`: the OCI runtime has completed create;
- `container_started`: the runtime reports that the source container process has
  started;
- `application_ready`: an optional application-declared readiness condition.

Only `rootfs_ready` for a lazy backend and the platform portion of
`container_created` can be approximately independent of logical image size.
Completion after `container_started` depends on the bytes the loader and
entrypoint touch.
`application_ready` is unbounded without an application contract.

## Image admission and catalog

Admission runs once for an immutable source digest:

1. resolve a mutable reference to an OCI manifest or index digest;
2. copy the selected platform manifest and blobs into same-region Artifact
   Registry under an Ignition-owned immutable repository;
3. verify registry identity, signature, provenance, and project policy;
4. validate OCI configuration, layer descriptors, diff IDs, whiteouts, path
   normalization, hardlinks, symlinks, xattrs, ownership, and allowed special
   files;
5. scan for prohibited configuration and known vulnerabilities according to
   policy;
6. statically validate documented GKE image-streaming requirements and record
   eligibility or a specific incompatibility; actual streaming is observed only
   at launch because GKE exposes no admission-time mount API;
7. asynchronously create candidate alternative representations needed for the
   custom GCE runtime; and
8. atomically publish the signed catalog record.

The catalog record contains:

```text
source manifest digest and selected platform
admitted OCI config digest
registry location and residency
available delivery representations and their immutable digests
filesystem semantic digest
logical, compressed, metadata, and object counts
streaming eligibility and incompatibility reason
access-profile IDs
security domain and encryption-key version
qualified runtime/kernel/backend tuples
revocation state and policy revision
```

The filesystem semantic digest covers the canonical flattened result, including
path, type, content digest, size, mode, uid, gid, hardlink identity, symlink
target, permitted xattrs, and permitted device metadata. The signed catalog also
covers the complete OCI execution configuration. A converted data representation
is accepted only if a differential extractor proves that its canonical tree and
execution configuration match the admitted source.

Generic image acceleration does not add `/ignition/init`, rewrite the entrypoint,
or wrap the source process. The existing GKE sandbox design's hard-coded
supervisor is a separate runtime concern and must not be used to claim support
for arbitrary images. A future opt-in managed lifecycle representation may add a
supervisor, but it has different PID 1 and signal semantics and requires a
separate public contract and qualification suite.

Tenant code receives neither registry credentials nor backend object-store
credentials.

Catalog publication uses these states:

```text
RESOLVING -> IMPORTING -> VERIFYING -> READY_BASE
                                      -> REJECTED

READY_BASE -> BUILDING_REPRESENTATION -> QUALIFYING -> READY
                                                  -> FAILED

READY_BASE or READY -> REVOKED
```

`READY_BASE` permits the admitted source representation to run on GKE.
Alternative GCE representations and profiles never delay it. Each transition is
idempotent and fenced by `(source digest, platform, policy revision, generation)`.
Only a catalog transaction that publishes a verified immutable digest makes a
representation selectable.

## GKE implementation

GKE owns the containerd and rootfs streaming path. Ignition must not depend on a
custom snapshotter, host FUSE mount, undocumented `gcfsd` API, or direct mutation
of the GKE-managed node runtime.

### Standard path

1. Schedule only the admitted source digest from same-region Artifact Registry.
2. Require an eligible containerd node image and enable GKE image streaming.
3. Because GKE does not expose the internal remote-rootfs mount timestamp, record
   successful image-pull completion immediately before container creation as the
   managed-path `rootfs_ready` proxy. Record container creation separately and
   never infer streaming from image size.
4. Record whether the launch streamed, used a node-local image, or fell back to a
   full pull from Kubernetes events and measured timings.
5. If the image is ineligible for streaming, either use an admitted eager pull or
   reject it when the request deadline cannot accommodate the measured eager
   path. The decision is explicit in sandbox status.

GKE streaming can introduce remote-read latency when startup touches many files,
downloads the complete image into the node cache in the background, and can
surface stale file handles after the managed streaming service restarts. These
are backend properties, not conditions Ignition can repair inside a running Pod.
An affected sandbox fails with `IMAGE_UNAVAILABLE`; retry creates a new sandbox.

### Secondary boot-disk cache cohorts

For stable, frequently launched images, CI builds a versioned GKE secondary
boot-disk image in `CONTAINER_IMAGE_CACHE` mode:

- cache large, shared, immutable OCI layers selected by measured launch demand;
- include multiple compatible images in one cache epoch where supported;
- create a new disk image and node pool to update content; existing node pools
  are never mutated in place;
- identify the epoch in the catalog and select its node pool using a server-owned
  node selector; and
- roll epochs blue/green while retaining an uncached GKE streaming pool.

Secondary boot disks accelerate node cold starts as well as Pod starts because
GKE creates them from disk images in parallel with node provisioning. They are a
coarse cache for predictable demand, not the per-image default and not suitable
for high-churn arbitrary images.

## Custom GCE implementation

The custom GCE path is deferred until the gate in
[GKE Sandbox](ignition-design-gke-sandbox.md#relationship-to-the-custom-runtime)
is met. When enabled, the first implementation uses a qualified upstream remote
snapshotter.

### Backend qualification order

1. **Nydus RAFS v6/EROFS:** preferred candidate for a merged tree, chunk-level
   reads and deduplication, prefetch, local blob cache, and an in-kernel hot path.
2. **eStargz:** OCI-compatible fallback with file/chunk metadata, chunk integrity,
   and prioritized-file prefetch.
3. **SOCI:** index-based fallback when preserving the source gzip layers is more
   valuable than conversion.
4. **Eager overlayfs:** mandatory compatibility fallback.

Qualification covers the pinned worker kernel, containerd content/snapshot APIs,
overlay arrangement, gVisor gofer, image features, and failure semantics. A
backend is never selected merely because its index can be mounted.

### Mount and cache topology

```text
gVisor sentry
  -> gofer
  -> writable per-sandbox upper
  -> qualified immutable lower
  -> Linux page cache
  -> node Local SSD/NVMe backend cache
  -> optional zonal read-through cache
  -> same-region registry or object storage
```

`ignition-hostd` performs only typed mount and unmount operations and verifies
that the mounted representation digest matches the catalog. Containerd remains
the content and snapshot service where the selected backend integrates with it.
No tenant-controlled path, mount option, backend URL, or credential reaches the
privileged API.

Local SSD is an ephemeral cache. Loss is a cache miss, never data loss. A zonal
cache is introduced only after measurements show that it improves cold-cache
p95 or reduces origin load enough to justify another serving tier.

## Adaptive access profiles

Access profiles are optional performance hints. They never restrict which files
or byte ranges a container may read.

A profile is keyed by:

```text
image digest
argv and working directory
non-secret environment policy hash
runtime/backend tuple
optional readiness contract revision
profile schema version
```

On sampled cold launches, the backend or gofer records ordered file byte ranges
read from `container_created` until `application_ready`, or for a fixed bounded
window when no readiness contract exists. The builder aggregates multiple
successful launches and publishes a candidate only after replay on an empty
cache. One anomalous trace cannot replace the active profile.

At launch:

- authenticated metadata is fetched first;
- high-confidence ranges are prefetched concurrently with container creation;
- adjacent remote ranges are coalesced to limit request amplification;
- demand faults always outrank speculative prefetch;
- prefetch has byte, concurrency, CPU, and time budgets; and
- a profile miss behaves as an ordinary lazy read.

Profiles with unstable coverage or a working set near the complete image are
disabled. Those images are candidates for eager pull or a secondary boot-disk
cache rather than increasingly aggressive prefetch.

## Strategy selection

The catalog stores exponentially weighted observations by image, profile, cache
state, backend, node class, and region. The selector predicts:

```text
lazy_cost = metadata_time
          + predicted_miss_bytes / effective_read_bandwidth
          + predicted_remote_request_count * request_latency
          + decompression_cpu_time

eager_cost = missing_compressed_bytes / pull_bandwidth
           + decompression_and_unpack_time
```

The prediction is advisory. Hard eligibility, security, disk-capacity, deadline,
and runtime-compatibility checks run first. Unknown stream-eligible images start
with lazy delivery and bounded readahead. The selector explores alternatives only
on sampled launches and never changes strategy within a running sandbox.

Every selection records its inputs, prediction, actual stage timings, bytes, and
fallbacks. A backend is automatically disabled for a compatibility class after a
bounded error-rate or latency regression threshold is exceeded.

## Integrity and confidentiality

- Verify the source OCI descriptor chain and the signature on the catalog.
- Verify alternative representation metadata before mount and data chunks as
  they enter a trusted cache. Use the backend's authenticated chunk metadata.
- Protect stable local cache entries with read-only ownership and fs-verity where
  the selected backend and kernel support it. Kernel page-cache hits are not
  redundantly rehashed on every read.
- Partition private caches and metrics by project security domain. Private
  content never uses cross-project presence checks or timing-visible dedup.
- Permit fleet-wide dedup only for artifacts on a signed public-content allowlist
  anchored to an exact digest and redistribution policy. Content similarity does
  not make an artifact public.
- Scope object-read and key-decrypt authority separately and bind both to the
  node's active project lease. Key version and encryption scheme are part of the
  catalog representation record.
- On revocation, prevent new mounts immediately. Existing sandboxes follow the
  incident policy; immutable cached bytes need no in-place mutation.

## Failure behavior

- catalog or signature unavailable before create: fail with
  `IMAGE_UNAVAILABLE`;
- digest, metadata, or chunk verification failure: discard the cache entry,
  retry once from an independent tier, then fail closed;
- remote fetch exhaustion: return an I/O failure and fail the affected sandbox;
- remote snapshotter, Nydus daemon, FUSE daemon, or GKE streaming service loss:
  fail affected sandboxes and recreate them; remounting a new filesystem at the
  same path does not repair existing mount and file references;
- local cache corruption: quarantine the cache device after repeated independent
  verification failures;
- strategy-specific incompatibility discovered before process start: retry once
  with the admitted eager representation if the request deadline permits;
- failure after the process starts: do not switch its rootfs backend in place.

## Feasibility assessment

| Capability | GKE | Custom GCE | Assessment |
|---|---|---|---|
| Lazy arbitrary OCI rootfs | Managed image streaming for eligible images | Nydus/eStargz/SOCI | Feasible without new filesystem |
| Immutable digest admission | Ignition import and catalog | Same catalog | Straightforward control-plane work |
| Profile-guided prefetch | No supported control of GKE streaming internals | Supported by candidate backends | GCE-only optimization initially |
| Warm cache on new nodes | Secondary boot-disk image cache | Baked disk or Local SSD warming | Feasible for stable popular sets |
| Chunk-level cross-image dedup | Managed and opaque | Nydus candidate | Private domains must remain partitioned |
| Transparent daemon recovery | Not exposed | Existing mounts cannot be replaced transparently | Explicitly unsupported |
| Size-independent application readiness | No | No | Impossible for arbitrary programs |

The main engineering risk is integration and operations, not data-structure
invention: representation conversion, differential semantic verification,
containerd/gVisor compatibility, cache isolation, and representative performance
testing. Nydus/EROFS remains a candidate until it passes those tests; this design
does not label it production-ready by assertion.

## Observability

Measure per launch:

- resolve, metadata fetch, mount, container create, container start, and
  optional application-ready latency;
- selected and actual backend, cache cohort, and fallback reason;
- logical image bytes, compressed bytes, remotely fetched bytes, decompressed
  bytes, and unused fetched bytes;
- requests, range sizes, cache hits, and latency by tier;
- prefetch precision, coverage, lateness, and interference with demand reads;
- decompression CPU, gofer/snapshotter CPU, memory, and Local SSD occupancy;
- integrity failures, backend restarts, stale handles, and sandbox failures; and
- predicted versus actual lazy and eager cost.

Do not put project or private image identity in globally visible metrics. Use
bounded-cardinality internal identifiers with project-authorized drill-down.

## Acceptance

1. Differential extraction proves every qualified representation is semantically
   equivalent to the admitted OCI source across whiteouts, opaque directories,
   links, xattrs, ownership, permissions, sparse files, and supported special
   files.
2. Shell-less and `scratch` images execute their original entrypoint and argument
   combination as PID 1 without an injected binary or shell.
3. With provider-side streaming metadata available and an empty node cache, a
   stream-eligible 100 GB image reaches `rootfs_ready` after authenticated
   metadata without transferring the complete image first; first-ever provider
   ingestion is measured separately.
4. Random and sequential reads across large files transfer only the backend's
   covering chunks or spans plus measured bounded amplification.
5. Images with unpredictable access remain correct without a profile.
6. An image that reads nearly all data selects eager delivery when measured eager
   cost is lower.
7. Cold-cache tests cover 1%, 10%, 50%, and 100% working sets; small-file storms;
   random `mmap`; deep layer stacks; concurrent starts; and cache eviction.
8. Corrupt manifests, indices, chunks, cache entries, and registry responses fail
   closed and never expose unverified bytes.
9. Private content and cache-hit observations do not cross project security
   domains. Only digest-allowlisted public content deduplicates globally.
10. Killing a filesystem or snapshotter daemon produces a bounded failure and
   sandbox recreation; no test expects an in-place remount to repair the sandbox.
11. GKE tests exercise streaming, eager fallback, secondary boot-disk cohorts,
    service quota exhaustion, and managed-service restart behavior.
12. GCE tests pin kernel, gVisor, containerd, and backend versions and complete
    1,000 create/read/exec/delete cycles without leaked mounts or cache references.
13. A custom filesystem project is rejected unless a written benchmark identifies
    an unmet requirement, reproduces it against all qualified upstream backends,
    and estimates implementation and long-term operations cost.

## Rollout

1. Implement immutable digest admission, same-region import, eligibility checks,
   catalog publication, and stage timing on the shipped GKE runtime.
2. Benchmark GKE streaming against eager pulls using a corpus of arbitrary real
   images and the acceptance workload matrix.
3. Add versioned secondary boot-disk cache cohorts for stable popular images.
4. Build the representation differential verifier and evaluate Nydus, eStargz,
   and SOCI offline.
5. If the custom GCE runtime gate is met, integrate the winning backend behind a
   per-image strategy flag with eager fallback.
6. Add sampled access profiles and adaptive selection only after baseline
   correctness and metrics are stable.
7. Reconsider a zonal cache or custom filesystem only from measured evidence.

## Upstream references

- [GKE image streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
- [GKE secondary boot-disk image cache](https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading)
- [Modal lazy container loading](https://modal.com/blog/jono-containers-talk)
- [Nydus](https://github.com/dragonflyoss/nydus)
- [eStargz format](https://github.com/containerd/stargz-snapshotter/blob/main/docs/estargz.md)
- [SOCI snapshotter](https://github.com/awslabs/soci-snapshotter)
- [Linux FUSE passthrough](https://docs.kernel.org/filesystems/fuse-passthrough.html)
