# Ignition Images and Startup Acceleration Design

**Status:** Proposed — the shipped GKE system uses image streaming but does not
yet implement immutable image admission, an image catalog, adaptive delivery, or
GKE Pod snapshot orchestration.

> This design covers arbitrary OCI containers. Fast image delivery must not
> depend on a particular application, language, framework, readiness hook, or
> data layout. GKE Pod snapshots and custom-runtime snapshots are optional
> accelerators for separately qualified workloads; they are not prerequisites
> for fast generic container startup.

**Parent:** [Ignition Technical Design](ignition-technical-design.md)
**Data layer:** [Image Data Layer](ignition-design-image-datalayer.md)
**Snapshot details:** [Checkpoint and Restore](ignition-design-checkpoint-restore.md)
**Class-specific accelerator:** [Fast Startup on GCP](ignition-design-fast-startup-gcp.md) — stratified delivery and managed Pod snapshots for inference images

## Scope

This document defines:

- immutable OCI image admission and regional import;
- selection among managed streaming, cached, lazy, and eager delivery;
- optional access-profile generation and prefetch;
- startup-stage definitions and objectives;
- image and startup artifact invalidation;
- the boundary between generic image startup and optional GKE Pod or custom
  process snapshots; and
- a measured gate for moving from GKE to a custom GCE runtime.

Persistent volumes, application datasets, and runtime/session recovery are out of
scope. Golden startup snapshots are available only to explicitly qualified
workloads and must pass their independent feasibility gate.

## Design principles

1. **Correct without a profile.** Every admitted image must work with arbitrary
   file access. A profile changes only prefetch order.
2. **Optimize the measured bottleneck.** Mount, container start, and
   application readiness are different measurements.
3. **Prefer managed and upstream implementations.** Do not build a filesystem
   merely to obtain chunk reads, deduplication, or prefetch.
4. **Keep an eager fallback.** Lazy delivery is harmful when startup reads most
   of the image or issues a remote small-file storm.
5. **Pin every identity.** Source images, converted representations, profiles,
   runtimes, and snapshots are immutable and content-addressed.
6. **Do not promise impossible bounds.** Arbitrary application initialization is
   not controlled by the platform.

## Layer boundary: generic path vs. opt-in accelerators

No single mechanism bounds startup time for an arbitrary image. The fast path
for *any* admitted container is a stack of layers that require no per-image
cooperation and apply to every image. Optional accelerators sit above that
stack and apply only to images that opt in and pass qualification. A design or
metric must not average opt-in accelerator performance into the generic-path
objective, and the generic path must never regress in the name of an opt-in
accelerator.

| Layer | Mechanism | Requires image cooperation | Applies to |
|---|---|---|---|
| Scheduling latency | Warm-node buffer / balloon Pods | No | GPU by default; CPU is independently opt-in with `IGNITION_MIN_WARM_CPU` / `IGNITION_MAX_WARM_CPU` ([GKE Sandbox — Warm capacity mechanism](ignition-design-gke-sandbox.md#warm-capacity-mechanism), [Default runtime](ignition-design-default-runtime.md#boundaries)) |
| Rootfs (default) | GKE image streaming, lazy | No | Every stream-eligible admitted image |
| Rootfs (fallback) | Eager pull, cost-selected | No | Any image, chosen by measured cost — see [Adaptive strategy selection](#adaptive-strategy-selection) |
| Rootfs (shared) | Secondary boot-disk cache epoch | No | Popular or content-sharing image sets, not unique/high-churn images |
| Startup hint | Access-profile prefetch | No — advisory only, never required for correctness | Sampled images with stable access patterns |
| Process/GPU state | Golden Pod snapshot | **Yes** — lifecycle contract, no request-derived state at capture, cross-node qualification | Explicitly opt-in, qualified images only — never the generic path |

The first four rows are the complete answer to "make any container start fast":
warm capacity removes node-provisioning latency, lazy streaming removes the
image-size dependency from `rootfs_ready`, the eager fallback keeps images that
read most of themselves from paying streaming's small-file penalty, and the
cache epoch accelerates whatever is actually popular without asking any image
to change. None of the four requires an image to declare anything, run a
readiness hook, or avoid request-derived state.

Application initialization — framework import, `cuInit()`, weight loading — is
explicitly out of bounds for the generic path per principle 6 above; access
profiles narrow it, they do not bound it. Golden Pod snapshots
([Optional golden startup snapshots](#optional-golden-startup-snapshots)) and
the class-specific stratified accelerator in
[Fast Startup on GCP](ignition-design-fast-startup-gcp.md) go further, but only
for images that opt into the lifecycle contract and pass qualification; they
are accelerators layered on this stack, not a replacement for it, and a
snapshot-qualified image that falls back to cold start must still land on the
same generic path above, not fail.

## Components

- `ignition-artifacts`: authoritative image, representation, profile, and
  optional snapshot catalog;
- `ignition-builder`: isolated import, validation, conversion, differential
  verification, profile generation, and optional snapshot workflow;
- `ignition-controller`: GKE delivery-policy and cache-cohort selection;
- `ignition-hostd`: deferred GCE typed mount/runtime broker;
- GKE image streaming and secondary boot-disk image caches on the shipped path;
- GKE Pod snapshots for optional managed rootfs/process/GPU restore;
- qualified Nydus, eStargz, SOCI, or eager overlayfs on the deferred GCE path;
- node page cache and backend-managed Local SSD cache where supported; and
- Artifact Registry and GCS for authoritative immutable artifacts.

## Startup stages

Record these timestamps separately:

```text
request_admitted
image_resolved
worker_assigned
rootfs_ready
container_created
container_started
runtime_ready
application_ready (only when declared)
```

- `rootfs_ready` means authenticated metadata is mounted for lazy delivery or
  the rootfs is completely unpacked for eager delivery.
- `container_started` means the runtime reports that the original OCI process
  has started.
- `runtime_ready` means required platform isolation and routing checks are ready;
  it does not imply the customer program is ready.
- `application_ready` exists only when the image/startup revision declares a
  health contract.

Logical image size need not dominate `rootfs_ready` under lazy delivery. It can
still dominate completion after `container_started` or `application_ready` when
the process reads most of the image. No design may call arbitrary application
startup independent of image size.

## Image admission

Admission is mandatory before an image can run:

1. resolve the requested reference to an immutable OCI manifest or index digest;
2. select and pin the required platform manifest;
3. copy manifests and blobs to an Ignition-owned same-region Artifact Registry
   repository;
4. verify registry identity, signature, provenance, and project policy;
5. validate OCI configuration and flattened filesystem semantics;
6. scan according to the current security policy;
7. statically validate documented GKE image-streaming requirements and record
   eligibility or a specific incompatibility; observe the actual streamed or
   eager path at launch;
8. publish the source representation and catalog state atomically; and
9. asynchronously build and verify optional representations and profiles.

Mutable tags are accepted only as import inputs. A sandbox always references the
admitted digest. Tenant code receives no registry, catalog, cache, or object-store
credentials.

Generic image acceleration preserves the original entrypoint, command,
environment, user, working directory, PID 1, and signal behavior. A
canonical-tree differential verifier rejects any difference in file metadata,
links, xattrs, special files, or OCI configuration. Shell-less and `scratch`
images are supported.

### Runtime compatibility dependency

**Status: partially implemented.** `CreateSandbox.nativeEntrypoint` (opt-in,
default `false`) makes the GKE launcher run the container's own OCI
`Entrypoint`/`Cmd` unchanged — no `Command` override, so PID 1 is exactly what
the image declares — and drops sandbox-init's `/healthz`/`/readyz` probes, so
public readiness falls back to kubelet's default (`Running` ⇒ `Ready`, since
there is no supervisor to probe). This closes the PID 1 blocker for a client
that already knows its image has no `sandbox-init`.

What remains before generic *admission* (a client handing over an arbitrary
registry reference with no prior knowledge) can default to this path: nothing
today inspects the admitted image to learn whether it embeds `sandbox-init`, so
the client must set the flag itself; there is still no catalog, so `imageId`
is not yet a pinned digest (see [Image Data Layer](ignition-design-image-datalayer.md));
and exec, idle tracking, and lifecycle hooks are simply unavailable on a
`nativeEntrypoint` sandbox rather than degrading through an alternate
mechanism — that gap is unchanged from before.

The sandbox Pod's security context (`runAsNonRoot: true`,
`readOnlyRootFilesystem: true`, all capabilities dropped, no service-account
token) is uniform across both entrypoint modes and is not relaxed for
`nativeEntrypoint`. This is a separate blocker from the three above, and a
sharper one for *generic* admission specifically: most public third-party
images either run as root by default (no `USER` directive, which
`runAsNonRoot` rejects at container-create) or write somewhere under their own
root filesystem at startup (which `readOnlyRootFilesystem` rejects at
runtime), so an arbitrary image handed to `nativeEntrypoint` today commonly
fails to start rather than running with reduced isolation. Loosening either
flag is not a generic-admission unblock to take lightly — it changes the
isolation posture uniformly for every sandbox on the platform, GPU and CPU,
managed and native — and should go through its own security review rather
than ride in with unrelated `nativeEntrypoint` work.

## Generic delivery strategies

### GKE managed streaming

This is the default for a stream-eligible image on the shipped runtime. GKE owns
the rootfs service and background download. Ignition observes it through
Kubernetes state, events, and stage timings; it does not rely on undocumented
`gcfsd` control or install a custom snapshotter on managed nodes.

An image that is not streamed uses an explicitly recorded eager pull. If its
measured pull cannot fit the sandbox startup deadline, creation fails early with
`IMAGE_UNAVAILABLE` rather than remaining ambiguously stuck.

### GKE secondary boot-disk cache

Use `CONTAINER_IMAGE_CACHE` secondary boot disks for stable sets of frequently
launched images or base layers. Cache images are immutable epochs produced by CI.
Changing an epoch creates a new node pool and rolls traffic blue/green. The
controller selects a cache cohort using server-owned scheduling directives and
always retains an uncached streaming pool.

This strategy accelerates known demand, including newly provisioned nodes. It is
not appropriate for every unique or rapidly changing customer image.

### Custom GCE lazy delivery

Only after the custom-runtime gate is met, benchmark Nydus RAFS v6/EROFS,
eStargz, and SOCI on the exact worker kernel, containerd, gVisor, and storage
path. Select a backend per qualified compatibility class. Do not assume one
backend wins for every small-file, random-read, sequential-read, deep-layer, or
high-concurrency workload.

The GCE implementation and custom-filesystem rejection gate are normative in
[Image Data Layer](ignition-design-image-datalayer.md).

### Eager delivery

Eager OCI pull and overlayfs unpack remain supported when:

- an image is incompatible with the available lazy backend;
- observed startup consumes most of the image;
- remote request amplification makes lazy delivery slower;
- sufficient image content is already present locally; or
- backend health policy disables lazy delivery.

### GKE Pod snapshot restore

For a snapshot-qualified container, GKE Pod snapshots are another managed startup
strategy. GKE saves whole-Pod process memory and filesystem changes to Cloud
Storage and restores a compatible Pod through GKE Sandbox. The gVisor kernel is
restored before application memory streams in the background; demand page faults
take priority over background loading.

This is not a universal image-delivery replacement:

- the Pod must use GKE Sandbox and a supported machine type;
- the distilled Pod spec, machine series, CPU architecture, gVisor kernel, and
  GPU driver where applicable must meet GKE's matching rules;
- persistent volumes are not captured;
- external connections terminate on restore;
- hostname, network identity, wall clock, per-instance identity, secrets, and
  environment-derived state require application-safe refresh; and
- the restored Pod may be `Running` before its hot memory working set has arrived.

If GKE finds no compatible ready snapshot, it cold-starts the Pod. Ignition treats
that as an observed strategy fallback, not as successful snapshot acceleration.

## Access profiles and prefetch

An access profile records ordered file byte ranges observed between
`container_created` and a declared readiness event or bounded observation
window. It is keyed by image digest, argv, working directory, non-secret
environment-policy hash, runtime/backend tuple, and readiness revision.

Profiles are learned from multiple successful sampled launches, replayed on an
empty cache, signed, and published immutably. At runtime they provide bounded
prefetch hints only. Demand reads have priority, and an access outside the
profile follows the normal backend read path.

GKE image streaming exposes no supported per-image prefetch API, so profiles on
GKE initially inform cache-cohort placement and eager-versus-streaming analysis.
On GCE they may drive the selected upstream backend's supported prefetch
mechanism.

## Adaptive strategy selection

Selection occurs before sandbox creation and does not change the rootfs backend
of a running process. Hard eligibility, security, compatibility, available disk,
and deadline constraints run before cost prediction.

For each `(image, startup key, backend, cache state, region, node class)`, retain
observed metadata latency, requested bytes, remote requests, throughput,
decompression CPU, unpack time, container start, and optional readiness. Use
those observations to compare:

```text
lazy = metadata + remote_bytes / throughput
     + remote_requests * request_latency + decompression

eager = missing_compressed_bytes / pull_throughput + decompress_and_unpack
```

Unknown eligible images begin with lazy delivery and conservative readahead.
Snapshot restore is considered only when the catalog contains a compatible,
verified snapshot and its measured restore cost beats normal image startup.
Exploration of another strategy is sampled and bounded. Automatically stop
selecting a backend for a compatibility class when its error rate or latency
crosses a configured rollback threshold.

## Optional golden startup snapshots

Snapshot restore is not guaranteed for an arbitrary image, but it is not tied to
a workload type. It is an independent, opt-in strategy for a snapshot-compatible
image/startup revision with no request-derived or instance-unique state at the
capture point.

A golden snapshot:

- is immutable and belongs to one project;
- is keyed by the exact image, distilled Pod spec inputs, startup revision,
  lifecycle contract, CPU/runtime compatibility tuple, and device tuple when
  applicable;
- captures only declared filesystem/process state;
- is verified by restore on another compatible worker before publication; and
- never substitutes for the admitted immutable rootfs.

### Managed GKE snapshot path

GKE Pod snapshots are generally available on GKE 1.35.3-gke.1234000 or later
(GKE release notes, May 6, 2026). GA removes the pre-GA dependency concern; it
does not remove Ignition's own qualification gate — cross-node restore,
correctness, isolation, and storage tests must pass before an artifact is
selectable. Normal image startup remains the production fallback and does not
depend on this feature. The composition-aware build, VRAM tiering, zonal cache,
and host-side prefetch for stratified inference images are specified in
[Fast Startup on GCP](ignition-design-fast-startup-gcp.md).

Use `PodSnapshotStorageConfig`, `PodSnapshotPolicy`, and `PodSnapshot` resources.
The controller must discover and use the API version served by the target cluster
rather than assume a CRD version. The cluster version must expose the required
resources and the chosen node class must appear in Google's current compatibility
list; both are recorded in the qualification tuple.
Prefer `federatedP4SA` path-scoped tokens for multi-tenant storage access so a
tenant container does not receive bucket credentials. Group snapshots by project
and immutable startup key. A workload trigger is used only when the application
implements the snapshot lifecycle contract; otherwise an authorized manual
trigger may capture a Pod after an external readiness and quiescence check.

After GKE reports the snapshot ready, restore it on another compatible node,
refresh identity and secrets, run health checks, and publish it in
`ignition-artifacts`. Production launch names the verified `PodSnapshot`
explicitly. GKE's automatic cold fallback is observed and reported; Ignition does
not mark the launch snapshot-accelerated unless restore telemetry confirms it.

GPU Pod snapshots are supported by current GKE documentation on listed GPU
machine types, including single-GPU L4 configurations. They copy GPU state into
process memory, so admission budgets peak host memory and stored bytes as well as
VRAM. Enablement still requires workload-specific cross-node restore and
correctness tests.

### Custom GCE snapshot path

Direct `runsc` plus NVIDIA checkpoint integration remains disabled until a
pinned custom-runtime tuple demonstrates repeatable process-tree checkpoint and
cross-host restore. The build/restore ordering and artifact rules in
[Checkpoint and Restore](ignition-design-checkpoint-restore.md) apply only to
that deferred path. Success of managed GKE Pod snapshots does not prove the
custom integration.

## Snapshot lifecycle contract

Snapshot-qualified workloads using the managed workload trigger implement:

```text
prepare_snapshot
after_restore
health
drain
abort
```

Hooks have strict deadlines and structured results. Generic image delivery does
not invoke or require them. A manual GKE snapshot may omit hooks only when an
external controller can prove readiness, quiescence, and post-restore correctness.
Failure to qualify a snapshot falls back on the admitted image delivery strategy
for future launches; it never makes the image unrunnable.

## Compatibility and invalidation

Create a new immutable catalog artifact when any covered input changes:

- OCI source or config digest;
- representation format, converter, or parameters;
- canonical filesystem semantics;
- access-profile key or schema;
- worker kernel, runtime, snapshotter, or gVisor compatibility tuple;
- startup lifecycle contract;
- encryption or signing policy; or
- GKE distilled Pod spec, managed runtime versions, or optional custom
  snapshot/device tuple.

Security or correctness revocation immediately prevents new placement. Running
sandboxes follow the incident policy. Caches contain immutable derived data and
are evicted; they are never edited in place.

## Failure behavior

- Missing, unsigned, revoked, or mismatched catalog data fails before node work.
- GKE streaming ineligibility takes the admitted eager path or fails explicitly.
- Backend fetch or verification exhaustion fails the sandbox with
  `IMAGE_UNAVAILABLE` and releases quota.
- Loss of a filesystem/snapshotter daemon fails affected running sandboxes;
  restarting and remounting at the same path is not transparent recovery.
- A strategy failure before process start may retry once with eager delivery if
  the request deadline permits. A running sandbox never changes rootfs backend.
- Optional snapshot failure falls back to normal image startup on a fresh
  sandbox and records the snapshot artifact as unhealthy.
- A restored Pod must refresh secrets and instance identity before readiness;
  failure to do so fails the sandbox rather than exposing captured credentials.

## Feasibility decision

The feasible near-term implementation is admission plus existing managed GKE
features. Image streaming requires control-plane work but no kernel modification.
Secondary boot-disk caches are feasible for stable popular images but require
versioned disk images and new node pools for updates. GKE Pod snapshots are GA
and technically feasible for qualified containers on supported tuples; they are
an optional qualification track rather than a prerequisite for generic startup.
They require snapshot CRD discovery and orchestration, Cloud Storage isolation,
compatibility-aware placement, state refresh, and functional verification.

The custom GCE path is feasible as a prototype because mature upstream lazy
backends exist. Production feasibility is conditional on exact containerd,
gVisor, overlay, kernel, registry, and failure-injection qualification. Building
and operating a new FUSE filesystem is not justified by the requirements in this
document.

Custom GCE snapshot feasibility remains unproven and cannot be placed on the
critical path of the generic startup project.

## Service objectives

The shipped GKE warm-capacity `runtime_ready` SLO remains defined in
[GKE Sandbox](ignition-design-gke-sandbox.md#startup-slo-and-definition). Add the
following objectives only after baseline measurements establish defensible
budgets:

- p95 `image_resolved -> rootfs_ready`, separated by streamed, cached, and eager;
- p95 `rootfs_ready -> container_started`, separated by runtime tuple;
- p95 platform overhead, excluding application execution;
- fetched-byte amplification relative to bytes requested before readiness; and
- backend failure rate and eager-fallback rate.

Do not define a universal `application_ready` SLO for arbitrary containers.
Images with a declared readiness contract may have a scoped SLO tied to their
image digest, startup key, and cache condition.

## Observability

Measure image import, validation, conversion, differential verification,
metadata size, streaming eligibility, selected backend, cache cohort, mount,
container start, bytes and requests by tier, decompression CPU, profile
precision/coverage, eager fallback, backend health, and optional readiness.

Keep the platform stages visible individually. A single cold-start histogram
cannot distinguish scheduling, image delivery, runtime creation, and arbitrary
application work.

## Acceptance

1. Every sandbox runs an immutable admitted digest; tags cannot change a running
   or pending sandbox.
2. Source and converted representations are semantically equivalent under the
   canonical differential suite, including unchanged OCI process configuration.
3. Shell-less and `scratch` images execute their original entrypoint and argument
   combination as PID 1 without requiring an injected binary or shell.
4. Arbitrary unprofiled reads work correctly on every selected backend.
5. With provider-side streaming metadata available and an empty node cache, a
   stream-eligible large image can reach `rootfs_ready` without full image
   transfer. First-ever provider ingestion is measured separately, and no test
   claims arbitrary application readiness is independent of bytes read.
6. Tests cover 1%, 10%, 50%, and 100% working sets, random and sequential reads,
   small-file storms, `mmap`, deep layers, sparse files, and concurrent starts.
7. Strategy selection chooses eager delivery when it is reproducibly faster and
   records prediction error.
8. GKE tests cover managed streaming, eager fallback, secondary boot-disk cache
   epochs, Pod snapshot cold fallback, quota exhaustion, and managed-service
   restart behavior.
9. GCE qualification compares Nydus, eStargz, SOCI, and eager overlayfs on the
   pinned production tuple before choosing a default.
10. Manifest, index, chunk, cache, and registry corruption fail closed.
11. Private content and cache observations never cross project security domains.
12. Daemon failure has bounded sandbox failure and cleanup; no in-place remount
    recovery is claimed.
13. Snapshots are selected only for compatible, verified startup keys; external
    connections, secrets, environment changes, persistent volumes, and
    instance-unique state pass explicit restore tests.
14. Direct custom-GCE snapshots remain disabled until their independent
    cross-host feasibility gate passes.

## Rollout

1. Implement digest admission, regional import, streaming eligibility, signed
   catalog publication, and startup-stage metrics on GKE.
2. Measure managed streaming against eager pulls over a representative corpus of
   arbitrary images.
3. Add secondary boot-disk cache epochs for stable high-demand image sets.
4. Qualify managed GKE Pod snapshots independently for selected compatible
   containers.
5. Evaluate alternative formats and the differential verifier offline.
6. Integrate a qualified lazy backend only if the custom GCE runtime gate is met.
7. Add access profiles and adaptive selection after correctness and baseline
   performance are stable.
8. Evaluate direct custom-GCE snapshots separately; they do not block the generic
   image-delivery rollout.

## Upstream references

- [GKE image streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
- [GKE secondary boot-disk image cache](https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading)
- [GKE Pod snapshots](https://cloud.google.com/kubernetes-engine/docs/concepts/pod-snapshots)
- [GKE Pod snapshot restore](https://cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots)
- [Nydus](https://github.com/dragonflyoss/nydus)
- [eStargz format](https://github.com/containerd/stargz-snapshotter/blob/main/docs/estargz.md)
- [SOCI snapshotter](https://github.com/awslabs/soci-snapshotter)
- [NVIDIA CUDA checkpoint](https://github.com/NVIDIA/cuda-checkpoint)
