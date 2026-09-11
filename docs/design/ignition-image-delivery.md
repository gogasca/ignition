# Ignition image delivery

**Status: PARTIAL / PROPOSED.** The shipped system delegates image delivery to
GKE image streaming and implements a **v0 admission slice**
(`internal/imagecatalog`, `POST/GET /v1/projects/{project}/images`). Everything
else here — the Ignition-owned catalog, adaptive lazy/eager delivery, secondary
boot-disk cache cohorts, access profiles, GKE Pod snapshot orchestration,
stratified delivery, and custom GCE lazy backends — is designed, not built.

**Parent:** [shipped architecture](ignition-shipped-architecture.md) ·
**Deferred runtime:** [deferred-runtime](ignition-deferred-runtime.md)

## 1. What ships today

- **Delivery:** GKE image streaming for eligible images on the sandbox node
  pools. GKE owns the containerd and rootfs path; Ignition observes it through
  Kubernetes state, events, and stage timings. Ignition installs no custom
  snapshotter and does not depend on undocumented `gcfsd` control.
- **v0 admission** (`POST /v1/projects/{project}/images`, `internal/imagecatalog`):
  takes `{imageId, sourceRef}`, resolves `sourceRef` **synchronously against the
  source registry** (no Ignition-owned copy, no `Idempotency-Key`, no transient
  `RESOLVING` state — a retry after success fails closed with `409
  IMAGE_ALREADY_EXISTS`), and returns the pinned record: `digest`, `registryRef`,
  `entrypoint`, `cmd`, `streamingEligible`. This is step 1 (resolve to a digest)
  and step 6 (static streaming-eligibility check) of the admission pipeline
  below.
- `imageId` on `CreateSandbox` is **not** yet a pinned digest — the controller
  resolves a bare path under the Artifact Registry sandbox prefix.
- Stage-latency metric `ignition_sandbox_stage_latency_seconds`.

### Security status

**Step 3 of the admission pipeline (verify registry identity, signature,
provenance, and project policy before resolution) is not implemented.** The v0
resolver does not copy into an Ignition-owned registry, verify signatures or
provenance, or scan.

**The network-reachability hole is closed** (`internal/imagecatalog/guard.go`):
`RemoteResolver` now rejects a `sourceRef` whose registry host is not on
`IGNITION_IMAGE_REGISTRY_ALLOWLIST` *before any network call*, dials through a
transport whose `Control` hook refuses every connection to a loopback /
unspecified / multicast / link-local (incl. `169.254.169.254`) / RFC1918 /
RFC4193 / `100.64.0.0/10` address — checked after DNS, so a rebinding hostname is
caught — bounds the whole resolution with `IGNITION_IMAGE_RESOLVE_TIMEOUT_SECONDS`
(default 30s, independent of the client connection), and returns a generic
`IMAGE_UNAVAILABLE` / `IMAGE_SOURCE_NOT_ALLOWED` to the client while logging the
underlying registry error server-side only (it was a topology oracle). The base
overlay sets the allowlist to the Artifact Registry host; an empty allowlist
logs a startup warning and imposes no *host* restriction (the address guard
still applies).

`POST /v1/projects/{project}/images` requires `image.create`, held by the same
project roles that already hold `sandbox.create`. This is a new *capability* (the
API server issuing outbound requests to a client-chosen host, including
`169.254.169.254`) reachable by an existing permission tier — not a new privilege
boundary. `ignition-api`'s Workload Identity holds no Artifact Registry role in
Terraform today, so the confused-deputy angle is not live — but nothing in code
prevents it, and the network-reachability exposure needs no registry credential.
The raw resolver error is returned to the client, distinguishing "nothing
listening" from "connection refused" from "TLS/auth failure" — a topology oracle
independent of whether resolution succeeds.

The interim mitigations above (host allowlist, address guard, resolve timeout,
error sanitisation) are in place. Step 3 proper — registry-identity / signature /
provenance / project-policy verification, and a same-region Ignition-owned copy
(step 2) so resolution stops depending on the source registry at all — is still
outstanding.

## 2. Admission pipeline (target)

Runs once per immutable source digest:

1. resolve a mutable reference to an OCI manifest/index digest; *(v0: done)*
2. copy the selected platform manifest + blobs into same-region Artifact Registry
   under an Ignition-owned immutable repository;
3. **verify registry identity, signature, provenance, and project policy;**
4. validate OCI config and flattened filesystem semantics (whiteouts, path
   normalization, hardlinks, symlinks, xattrs, ownership, special files);
5. scan for prohibited configuration and known vulnerabilities per policy;
6. statically validate GKE image-streaming requirements, record eligibility or a
   specific incompatibility (actual streaming is observed only at launch); *(v0: done)*
7. asynchronously build candidate alternative representations (custom GCE only);
8. atomically publish the signed catalog record.

Catalog states: `RESOLVING → IMPORTING → VERIFYING → READY_BASE → REVOKED`, and
`READY_BASE → BUILDING_REPRESENTATION → QUALIFYING → READY → REVOKED`.
`READY_BASE` lets the admitted source run on GKE; alternative representations
never delay it. Each transition is idempotent and fenced by `(source digest,
platform, policy revision, generation)`.

The catalog record carries: source + admitted config digests, registry location
and residency, delivery representations and their digests, the **filesystem
semantic digest** (canonical flattened tree — path, type, content digest, size,
mode, uid, gid, hardlink identity, symlink target, permitted xattrs/devices),
byte/object counts, streaming eligibility + reason, access-profile IDs, security
domain + encryption-key version, qualified runtime tuples, and revocation state.
A converted representation is accepted only if a differential extractor proves
its canonical tree and OCI execution configuration match the admitted source.

Generic acceleration never adds `/ignition/init`, rewrites the entrypoint, or
wraps the source process — the source process stays PID 1. Tenant code receives
no registry, catalog, cache, or object-store credentials. Mutable tags are
import inputs only; a sandbox always references the admitted digest.

## 3. Startup stages

Record separately: `request_admitted`, `image_resolved`, `worker_assigned`,
`rootfs_ready` (authenticated metadata mounted for lazy, or fully unpacked for
eager), `container_created`, `container_started` (OCI process started),
`runtime_ready` (platform isolation/routing checks pass), `application_ready`
(only when the image declares a health contract).

Only `rootfs_ready` for a lazy backend and the platform part of
`container_created` can be roughly image-size-independent. Completion after
`container_started` depends on the bytes the loader and entrypoint touch;
`application_ready` is unbounded without an application contract. **No design may
call arbitrary application startup independent of image size**, and no metric may
average opt-in accelerator performance into the generic-path objective.

## 4. Generic delivery strategies (PROPOSED)

The fast path for *any* admitted container is a stack that needs no per-image
cooperation:

| Layer | Mechanism | Applies to |
|---|---|---|
| Scheduling latency | warm-node buffer / balloon Pods ([shipped architecture §7](ignition-shipped-architecture.md#7-warm-node-capacity)) | GPU by default; CPU opt-in |
| Rootfs (default) | GKE image streaming, lazy | every stream-eligible admitted image |
| Rootfs (fallback) | eager pull, cost-selected | any image, by measured cost |
| Rootfs (shared) | secondary boot-disk cache epoch (`CONTAINER_IMAGE_CACHE`) | popular / content-sharing image sets, not unique or high-churn images |
| Startup hint | access-profile prefetch — advisory, never required | sampled images with stable access patterns |

- **Eager fallback** is used when the image is lazy-incompatible, startup reads
  most of the image, remote request amplification makes lazy slower, enough
  content is already local, or backend health disables lazy. If a measured eager
  pull cannot fit the sandbox startup deadline, creation fails early with
  `IMAGE_UNAVAILABLE` rather than remaining ambiguously stuck.
- **Secondary boot disks** are immutable epochs built by CI; changing an epoch
  creates a new node pool and rolls blue/green while retaining an uncached
  streaming pool. They accelerate node cold starts too (provisioned in parallel
  with the node).
- **Access profiles** record ordered file byte ranges between `container_created`
  and readiness, keyed by image digest + argv + working dir + non-secret env
  policy hash + runtime tuple + readiness revision; learned from multiple sampled
  launches, replayed on an empty cache, signed, published immutably. GKE streaming
  exposes no per-image prefetch API, so on GKE profiles initially inform
  cache-cohort placement and eager-vs-streaming analysis.
- **Strategy selection** happens before sandbox creation, never mid-run. Hard
  eligibility / security / disk / deadline / compatibility checks run first, then
  a cost comparison (`lazy_cost` = metadata + miss_bytes/bandwidth +
  requests·latency + decompression; `eager_cost` = missing_compressed_bytes/pull
  + unpack). Unknown eligible images start lazy with bounded readahead. A backend
  is auto-disabled for a compatibility class after a bounded error/latency
  regression.

## 5. GKE Pod snapshots (PROPOSED)

GKE Pod Snapshots are **GA** (GKE ≥ 1.35.3-gke.1234000, May 6 2026): whole-Pod
process memory + rootfs delta + `emptyDir`/`tmpfs` + GPU state (via
`cuda-checkpoint`) to Cloud Storage; the gVisor kernel is restored first and
memory streams in the background with demand faults prioritized. Requires GKE
Sandbox and a supported machine type (`g2-standard-4…96`, `a2/a3` single-GPU;
MIG unsupported). Persistent volumes are not captured; external connections
terminate; hostname / network identity / clock / secrets / env-derived state need
application-safe refresh; a restored Pod may be `Running` before its hot working
set arrives.

Ignition does **not** yet create, qualify, select, or restore them. When built:
group by project + immutable startup key; use a workload trigger only when the
app implements the lifecycle contract (`prepare_snapshot`, `after_restore`,
`health`, `drain`, `abort`), else an authorized manual trigger after an external
quiescence check; restore on a second compatible node, refresh identity/secrets,
run health checks, publish in `ignition-artifacts`, and name the verified
`PodSnapshot` explicitly at launch. GKE's automatic cold fallback is observed and
reported — never counted as snapshot acceleration. Invalidate when the node
pool's gVisor kernel or NVIDIA driver version changes (GKE requires an exact
match for GPU snapshots). Normal image startup stays the production fallback and
does not depend on this.

## 6. Stratified delivery — `ignition-strata` (PROPOSED)

A class-specific accelerator for allowlisted inference. Large ML images couple
base OS/CUDA/framework (~8 GB, widely shared), application (~1 GB, per image),
and model weights (tens of GB, often shared) — every delivery mechanism moves
that blob before the loader can even start. `ignition-strata` runs inside
admission, after signature/provenance/policy verification, and produces a
**delivery representation** + **composition record** that must pass the same
differential verifier (flattened tree and OCI config unchanged except recorded S2
path removals; source process PID 1).

- **Classify** each file by content against a regional **corpus index** (built by
  running `nydus-image create` over each admitted image as an *offline analyzer
  only* — no `nydusd`, no snapshotter, no node component): **S0 base** (in the
  top-K most-shared file sets, by content so different Dockerfiles installing the
  same wheel share it), **S2 weight/data** (large files under declared data paths
  or matching weight-format signatures), **S1 application** (everything else,
  target ≤ 2 GB).
- **Re-layer S0+S1** into a new OCI manifest over the same flattened tree with
  **canonical, deterministic layer boundaries** so the base-layer digest is
  byte-identical across images — GKE image streaming's layer cache and a
  content-derived `CONTAINER_IMAGE_CACHE` epoch then hit regardless of the
  customer's layering.
- **Publish S2** as **ReadOnlyMany Persistent Disks** (pd-ssd ≤ 100 readers;
  Hyperdisk ML ≤ 2,500 on A3-class, not G2), keyed by content digest and mounted
  read-only into the Pod at the original paths by the server-owned spec. Weights
  are never inside the delivery image and never file-data in a Pod snapshot. This
  is the one narrowing of the "no application datasets" scope exclusion —
  read-only data *found inside the admitted image*; writable Volumes and
  customer-managed datasets stay out of scope.
- **Warm-pool planner** extends the warm-capacity controller with an attachment
  plan (which weight disks on which warm nodes), Anywhere Cache warming for hot
  snapshots, and `strata_ready` / `snapshot_ready` placement affinity labels.
- **Host-side prefetch:** `ignition-gpu-agent` (already privileged, one per node)
  reads the artifact's access profile on Pod admission and issues host
  `readahead` against S2 files and cached snapshot ranges before the sentry
  faults. This is the one place a profile drives prefetch on the shipped GKE
  path; demand faults still win.

No filesystem, snapshotter, CAS, supervisor injection, `snapshotd`,
`ignition-hostd`, direct `runsc` lifecycle, or `cuda-checkpoint` injection is
built here. Qualification target for a snapshot-qualified L4 inference image with
≤ 8 GiB captured VRAM on a warm affine node with a hot zonal cache: p95
`request_admitted → application_ready` ≤ 9s (≤ 30s cold cache).

## 7. Custom GCE lazy delivery (DEFERRED)

Deferred until the [custom-runtime gate](#8-the-custom-gce-gate) is met. The
first implementation uses a qualified upstream remote snapshotter — **no custom
`ignitionfs`**, which is not approved. Qualification order: **Nydus RAFS
v6/EROFS** (merged tree, chunk reads + dedup, prefetch, local blob cache,
in-kernel hot path), then **eStargz**, then **SOCI**, with **eager overlayfs** as
the mandatory compatibility fallback. Qualification pins the worker kernel,
containerd content/snapshot APIs, overlay arrangement, gVisor gofer, and failure
semantics — a backend is never selected just because its index mounts.

Mount/cache topology: `gVisor sentry → gofer → writable per-sandbox upper →
qualified immutable lower → page cache → node Local SSD → optional zonal
read-through cache → same-region registry`. `ignition-hostd` performs only typed
mount/unmount and verifies the mounted digest matches the catalog. Local SSD loss
is a cache miss, never data loss.

Private caches and metrics are partitioned by project security domain; private
content never uses cross-project presence checks or timing-visible dedup.
Fleet-wide dedup is permitted only for digest-allowlisted public content. Object-
read and key-decrypt authority are scoped separately and bound to the node's
project lease.

## 8. The custom GCE gate

The custom Compute Engine image path (and the rest of the [deferred
runtime](ignition-deferred-runtime.md)) is built only on measured evidence that
GKE cannot meet a requirement — any of:

1. the 9s warm-capacity SLO or the cold-node target cannot be met after
   image-streaming and warm-pool tuning;
2. managed GKE Pod snapshots fail the measured startup / lifecycle /
   compatibility / isolation / storage requirements after qualification;
3. driver/`nvproxy`/CUDA tuple pinning stricter than GKE's validated versions is
   required for snapshot portability;
4. per-GPU lease fencing or lifecycle control beyond Kubernetes semantics is
   demonstrably necessary.

A custom filesystem specifically is rejected unless a written benchmark
identifies an unmet requirement, reproduces it against all qualified upstream
backends, and estimates build + long-term operations cost.

## 9. Failure behavior

- Catalog / signature unavailable before create → `IMAGE_UNAVAILABLE`.
- Digest / metadata / chunk verification failure → discard the cache entry, retry
  once from an independent tier, then fail closed.
- Remote fetch exhaustion → I/O failure, fail the affected sandbox.
- Remote snapshotter / Nydus / FUSE / GKE streaming service loss → fail affected
  sandboxes and recreate; remounting at the same path does not repair existing
  mount/file references.
- Local cache corruption → quarantine the device after repeated independent
  verification failures.
- Strategy incompatibility discovered before process start → retry once with the
  eager representation if the deadline permits.
- Failure after the process starts → never switch its rootfs backend in place.
- Snapshot failure at any stage → fall back to the generic path on a fresh
  sandbox, mark the artifact unhealthy; no image becomes unrunnable.
- A restored Pod must refresh secrets and instance identity before readiness, or
  the sandbox fails rather than exposing captured credentials.

## 10. Acceptance (selected)

1. Differential extraction proves every qualified representation is semantically
   equivalent to the admitted OCI source (whiteouts, opaque dirs, links, xattrs,
   ownership, permissions, sparse files, special files) with unchanged OCI
   process configuration.
2. Shell-less and `scratch` images run their original entrypoint as PID 1 with no
   injected binary or shell.
3. With provider streaming metadata and an empty node cache, a stream-eligible
   100 GB image reaches `rootfs_ready` after authenticated metadata without
   transferring the whole image; first-ever ingestion is measured separately.
4. Cold-cache tests cover 1/10/50/100 % working sets, small-file storms, random
   `mmap`, deep layers, concurrent starts, cache eviction.
5. Strategy selection chooses eager when it is reproducibly faster; records
   prediction error.
6. Corrupt manifests / indices / chunks / cache entries / registry responses fail
   closed and never expose unverified bytes.
7. Private content and cache-hit observations never cross project security
   domains; only digest-allowlisted public content deduplicates globally.
8. Killing a filesystem or snapshotter daemon produces a bounded failure and
   sandbox recreation — no test expects an in-place remount to repair a sandbox.
9. Snapshots are selected only for compatible, verified startup keys; external
   connections, secrets, env changes, persistent volumes, and instance-unique
   state pass explicit restore tests.
10. GCE qualification compares Nydus / eStargz / SOCI / eager overlayfs on the
    pinned production tuple before choosing a default; 1,000 create/read/exec/
    delete cycles leak no mounts or cache references.
11. A custom filesystem project is rejected unless the benchmark gate in §8 is
    met.

## Upstream references

- [GKE image streaming](https://cloud.google.com/kubernetes-engine/docs/how-to/image-streaming)
- [GKE secondary boot-disk image cache](https://cloud.google.com/kubernetes-engine/docs/how-to/data-container-image-preloading)
- [GKE Pod snapshots](https://cloud.google.com/kubernetes-engine/docs/concepts/pod-snapshots) ·
  [restore](https://cloud.google.com/kubernetes-engine/docs/how-to/pod-snapshots)
- [GKE ReadOnlyMany persistent disks](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/readonlymany-disks) ·
  [Hyperdisk ML](https://docs.cloud.google.com/compute/docs/disks/hd-types/hyperdisk-ml)
- [Nydus](https://github.com/dragonflyoss/nydus) ·
  [eStargz](https://github.com/containerd/stargz-snapshotter/blob/main/docs/estargz.md) ·
  [SOCI](https://github.com/awslabs/soci-snapshotter)
- [NVIDIA CUDA checkpoint](https://github.com/NVIDIA/cuda-checkpoint) ·
  [gVisor issue #12600](https://github.com/google/gvisor/issues/12600)
- [Modal lazy container loading](https://modal.com/blog/jono-containers-talk)
