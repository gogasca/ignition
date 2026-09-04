# Ignition Image Data Layer Design (`ignitionfs`)

**Status:** Not implemented — design of record for the deferred custom lazy-image path.

> The shipped system has no image-admission catalog and no lazy image delivery.
> `ignition-controller` resolves a sandbox's `imageId` to
> `${IGNITION_SANDBOX_IMAGE_PREFIX}/{imageId}` and lets GKE image streaming pull
> it; sandboxes always cold-start from the OCI image. This document specifies the
> content-addressed FUSE data layer named `ignitionfs` in
> [Images and Startup Acceleration](ignition-design-images-startup.md#custom-filesystem-decision),
> for the case the MVP's curated-image assumption breaks: arbitrary, large
> (tens to hundreds of GB) customer images. It belongs to the custom Compute
> Engine runtime and is gated on the same evidence — see
> [Relationship to the custom runtime](#relationship-to-the-custom-runtime).

**Parent:** [Ignition Technical Design](ignition-technical-design.md)
**Sibling:** [Images and Startup Acceleration](ignition-design-images-startup.md) — golden startup snapshots, admission, strategy selection
**Sibling:** [Checkpoint and Restore](ignition-design-checkpoint-restore.md) — process/GPU memory snapshots
**Runtime boundary:** [Worker Runtime and GPU Isolation](ignition-design-worker-runtime.md) — `ignition-hostd`, gVisor, containerd content services

## Scope

Defines content-addressed image ingest, the chunk store and its cache tiers, the
per-node `ignitionfs` FUSE daemon, the sandbox rootfs mount, prefetch, cross-tenant
dedup and its confidentiality controls, and the integrity model.

Out of scope: golden CPU/GPU memory snapshots and the lifecycle-hook contract
([Images and Startup Acceleration](ignition-design-images-startup.md)); process and
GPU checkpoint ordering ([Checkpoint and Restore](ignition-design-checkpoint-restore.md));
writable persistent Volumes ([Storage and Volumes](ignition-design-storage-volumes.md)).
`ignitionfs` delivers the immutable image rootfs only.

## Problem statement

eStargz and SOCI are the MVP answer for lazy image delivery and remain the default
([Images and Startup Acceleration](ignition-design-images-startup.md#custom-filesystem-decision)).
They dedup at layer granularity, require a per-image conversion or index, and
materialize a whole file on first read — so a model server that `mmap`s a 40 GB
weight file pays the full transfer before the first inference, and two customer
images that share a 15 GB base but differ in one layer share nothing below the
layer boundary.

`ignitionfs` targets the regime where those properties dominate cost:

1. customers ship arbitrary images, not an allowlisted set, so the image is on the
   critical path of every cold start;
2. images are large (tens to hundreds of GB) and share substantial content
   (base OS, CUDA, framework wheels, common model shards);
3. loaders `mmap` large files and touch a fraction of the bytes.

The design goal is **cold-start latency independent of image size**: container
creation blocks on metadata only, and data transfers on demand at chunk
granularity from a tiered content-addressed cache.

## Components

- `ignitionfs` — per-node unprivileged FUSE daemon. Serves one read-only mount per
  sandbox from an in-memory manifest and a tiered chunk cache.
- `ignition-builder` ingest extension — flattens an admitted OCI image, chunks it,
  uploads missing chunks to the CAS, and publishes a signed image manifest.
- `ignition-artifacts` catalog extension — stores manifests, chunk references,
  prefetch profiles, and per-project chunk keys.
- Chunk store (CAS) — object-storage bucket of immutable, self-verifying chunks,
  with an optional per-zone read-through cache.

`ignition-hostd` owns the privileged mount setup and namespace wiring; `ignitionfs`
itself is unprivileged and holds no host credentials beyond a read-only CAS
identity. Containerd content and lazy-snapshot services are not on the image-data
path when `ignitionfs` is selected; the manifest and CAS are the source of truth.

## Addressing model

The manifest maps each path to an ordered list of chunk references. Chunking is
hybrid:

- files smaller than 64 KiB are stored whole as one chunk;
- larger files are split by **FastCDC** content-defined chunking, ~1 MiB average,
  256 KiB–4 MiB bounds;
- a chunk id is `sha256` of the chunk plaintext.

Content-defined boundaries keep dedup robust across inserts and shifts (model
shards re-exported with different headers, rebuilt wheels). Chunk granularity
below the file boundary is what enables partial reads and `mmap` fault-in.

Chunks are immutable and self-verifying. The cache therefore needs no invalidation
and is safely shared across images and — subject to
[Dedup and confidentiality](#dedup-and-confidentiality) — across tenants.

## Ingest

Runs once per admitted OCI digest, after signature, provenance, and policy
verification ([Images and Startup Acceleration](ignition-design-images-startup.md#image-admission)):

1. pull the image layers into an isolated scratch area;
2. **apply the layers to a flattened rootfs**, resolving whiteouts and opaque
   directories, so the manifest is layer-independent and dedup is not capped at
   the layer boundary;
3. walk the flattened tree:
   - regular file → FastCDC → `sha256` per chunk → conditional PUT of each chunk
     the CAS does not already hold;
   - symlink, directory, device, fifo, socket, hardlink, xattrs → metadata only;
4. emit the **image manifest**:
   - per path: type, mode, uid, gid, mtime, size, xattrs, and either the ordered
     `(chunk digest, offset, length)` list or the link target;
   - the OCI config subset the runtime needs: `Entrypoint`, `Cmd`, `Env`,
     `WorkingDir`, `User`;
   - a **Merkle root** over the sorted `(path → file digest)` set;
   - schema version, source OCI digest, chunker parameters, created-at;
5. sign the manifest and publish it to the catalog keyed by source OCI digest;
6. optionally build a [prefetch profile](#prefetch).

Ingest is O(image bytes) once. It is amortized across every launch of the image
and every other image that shares content. Tenant code never receives worker
registry or CAS credentials.

Ingest cost is comparable to producing an eStargz image or a SOCI index; the
manifest replaces the SOCI index.

## Chunk store and cache tiers

```text
read(chunk):
  ignitionfs page cache / node page cache      ~microseconds
  → node local NVMe CAS cache                   ~100 microseconds
  → per-zone read-through chunk cache (option)  ~1-5 ms
  → object storage (GCS)                        ~10-50 ms
```

- Object layout: `casv1/sha256/<first-2-hex>/<full-digest>`. Small chunks are
  packed into aggregate objects with a side index (as `casync` castr and git
  packfiles do) to bound per-object request cost; large chunks are stored
  individually.
- The node NVMe cache is content-addressed and evicts by least-frequently-used.
  It is sized to a small multiple of the working set. Because chunks are shared
  across images, warm-node hit ratio is high once a few images are resident.
- The per-zone cache is added only after measurement shows cross-node first-fetch
  latency or object-storage egress is material. It is itself a set of `ignitionfs`
  daemons in cache-only mode or an equivalent object cache; it is never
  authoritative.
- `ignitionfs` fetches chunks with a node Workload Identity scoped to read-only on
  the CAS bucket. It cannot write the CAS and has no other Google API scope.

## `ignitionfs` daemon

One daemon per node, one read-only FUSE mount per sandbox.

### Mount topology

`ignition-hostd` mounts the `ignitionfs` tree read-only at
`/var/lib/ignition/images/<sandbox>/lower` in the sandbox's mount namespace, then
composes the sandbox rootfs as an overlay: `ignitionfs` lower, an NVMe-backed
`upperdir` for writes, and a `workdir`. The gVisor gofer is given the overlay (or,
per benchmark, the `ignitionfs` lower directly with `runsc`'s own overlay on top).

The rootfs is read-only to the tenant except for policy-permitted writable paths
and `/scratch`, consistent with the
[GKE Sandbox](ignition-design-gke-sandbox.md#sandbox-pod-profile-normative) profile.

### Metadata path

The full manifest is loaded into daemon memory at mount. Even a 100 GB image has a
manifest of tens of MB — it is path-and-attribute data plus chunk-digest lists.
`lookup`, `getattr`, `readdir`, `readlink`, and `open` are served from memory with
zero I/O. `entry_timeout` and `attr_timeout` are set high because image metadata is
immutable for the life of the mount.

This is the property that lets container creation block on metadata, not on image
transfer: `runsc create` completes as soon as the manifest is resident.

### Data path

`read(fd, offset, length)`:

1. resolve the covering chunk set from the file's chunk list;
2. for each chunk, fetch through the tier stack;
3. verify the chunk against its `sha256` before use — always, on every tier
   including local NVMe;
4. serve the requested range; the decompressed chunk remains in page cache.

`mmap` is supported through the FUSE page-fault path: a fault becomes a `read` of
the faulted range, so a loader that maps a large weight file transfers only the
pages it touches. On a custom fleet, target a node kernel with `FUSE_PASSTHROUGH`
(6.9+) so reads that hit an already-local chunk file bypass the daemon entirely.

Readahead: sequential access prefetches the next N chunks; files named in the
prefetch profile fetch asynchronously on `open`.

### Integrity

- The manifest signature is verified at mount; an unverifiable or mismatched
  manifest fails the mount.
- Every chunk is verified against its digest on every read.
- The manifest Merkle root is recorded on the sandbox's compatibility row, so the
  runtime can assert the sandbox booted exactly the admitted image.
- A cache or object-store compromise cannot inject content: unverified bytes are
  never served.

### Failure behavior

- chunk fetch fails after bounded retries → `EIO` on the read → sandbox `FAILED`
  with `IMAGE_UNAVAILABLE`; quota released;
- daemon crash → mounts go stale → the node supervisor restarts the daemon and
  remounts from the manifest; all daemon state is derivable from the manifest and
  the CAS, so no sandbox state is lost that was not already lost with the process;
- manifest not resolvable at create time → sandbox `FAILED` with
  `IMAGE_UNAVAILABLE` before any node work.

### Implementation

The FUSE hot path is Rust (low-level FUSE API) to avoid GC pauses on reads and to
keep per-syscall overhead low. Ingest and control-plane code may be Go. A later,
more invasive option is to resolve file content from the CAS inside a custom gofer,
removing the kernel FUSE round trip; it is not the first version.

## Prefetch

- **Prefetch profile.** During admission, trial-boot the image under `runsc` to the
  init supervisor with `ignitionfs` in trace mode, record the ordered set of chunks
  read, and store it on the catalog entry. At launch, `ignitionfs` streams the
  profile chunks in parallel with `runsc create` so the working set is local by
  the time the entrypoint runs.
- **Node pinning.** The warm-pool controller pre-pulls the chunk working set of the
  top-K images by recent launch rate onto warm nodes, into the NVMe cache.
- **Zonal warm.** The per-zone cache, when present, is pre-populated from the same
  top-K list so the first node in a zone to launch an image does not pay full
  object-storage latency.

## Dedup and confidentiality

Content-addressed dedup across tenants is a side channel: a cross-tenant cache hit
reveals that some other tenant holds a file with a given hash. The response is
tiered by content class.

- **Public base content** (OS, CUDA, framework wheels, publicly distributed model
  weights): plaintext chunk digest, global dedup. No confidentiality is lost
  because the content is already public.
- **Customer-private content**: a per-project keyed tier. The chunk id is
  `HMAC(project_key, sha256(chunk))`, which disables cross-project dedup for
  private layers while public base layers still dedup globally. Chunks in this
  tier are additionally encrypted at rest with a per-project data key under
  envelope encryption and project/domain KMS wrapping, reusing the key hierarchy
  from [Checkpoint and Restore](ignition-design-checkpoint-restore.md).
- Ingest classifies each file: content that resolves to a known public base
  artifact goes to the public tier; everything else goes to the project tier. The
  classification is recorded in the manifest.

Object-read permission and decrypt permission on the private tier are separate
grants. `ignitionfs` on a node hosting a project's sandbox receives only that
project's decrypt grant for the duration of the mount.

## Interaction with gVisor

- The gofer performs host filesystem I/O on the sentry's behalf; it is pointed at
  the overlay whose lower is the `ignitionfs` mount. Sentry VFS sees ordinary
  files. A read traverses gofer → host kernel → FUSE → daemon; the local NVMe and
  page-cache tiers keep the common case off the daemon.
- `ignitionfs` replaces the eStargz/SOCI lazy snapshotter in the
  [Worker Runtime](ignition-design-worker-runtime.md) design when selected per
  image. Containerd is not used for image data in that configuration.
- `runsc` rootfs composition (kernel overlay vs `runsc` internal overlay vs
  gofer-direct on the `ignitionfs` lower) is a benchmarked choice, recorded per
  workload class alongside the
  [restore I/O strategy](ignition-design-images-startup.md#restore-io-strategy).

## Relationship to golden snapshots

`ignitionfs` and golden startup snapshots
([Images and Startup Acceleration](ignition-design-images-startup.md#golden-startup-snapshot))
are complementary and independently selectable per image/startup revision:

- `ignitionfs` removes image *size* from the cold-start critical path but does not
  remove framework import, CUDA context creation, kernel autotune, or weights
  reaching VRAM;
- a golden snapshot removes those by restoring a warmed process, but its build
  still needs the image rootfs, which `ignitionfs` delivers;
- for a large arbitrary image with no golden snapshot, `ignitionfs` plus a
  prefetch profile is the fastest available path;
- for an allowlisted stateless workload, the golden snapshot is faster and
  `ignitionfs` only accelerates the golden build and the clean-recreation path.

Strategy selection ([Images and Startup Acceleration](ignition-design-images-startup.md#strategy-selection))
gains `ignitionfs`-lazy as an explicit option to benchmark.

## Relationship to the custom runtime

This design is part of the deferred custom Compute Engine runtime and is gated on
the same evidence as the rest of it
([GKE Sandbox — Relationship to the custom runtime](ignition-design-gke-sandbox.md#relationship-to-the-custom-runtime)),
plus a data-layer-specific trigger: reproducible measurements on real customer
images showing that

1. images are large enough and arrive uncurated enough that image transfer is on
   the cold-start critical path after warm-pool and streaming tuning, **and**
2. cross-image content sharing is high enough that chunk-level global dedup
   materially cuts storage and first-fetch cost over layer-level dedup, **and**
3. whole-file materialization on `mmap` (eStargz/SOCI behavior) measurably delays
   model readiness for representative loaders.

Absent that evidence, eStargz or SOCI plus the NVMe and page-cache tiers is the
supported path and `ignitionfs` is not built. The public API, image-admission
contract, and startup targets are unchanged by this design.

## Observability

Per launch and per node, labeled by region, image, and project where applicable:

- manifest resolve and mount time;
- time to first instruction after `runsc create`;
- bytes fetched per launch, by cache tier, and chunk hit ratio per tier;
- `mmap` fault rate and fault-served bytes;
- prefetch profile coverage: fraction of first-N-seconds reads served from
  prefetched chunks;
- ingest time, chunk count, manifest size, and measured dedup ratio
  (unique bytes ÷ logical bytes) per image and fleet-wide;
- CAS object count, stored bytes, and per-tier storage;
- daemon restart count and stale-mount recoveries.

## Acceptance

- Container creation for a 100 GB image completes on manifest residency; measured
  time to `runsc create` completion is independent of image size within noise.
- A loader that `mmap`s a large weight file and reads a fraction of it transfers
  approximately that fraction, not the whole file.
- Two images sharing a large base but differing in one layer share their common
  chunks in the CAS and in the node cache; measured dedup ratio reflects it.
- Every chunk is digest-verified on every read from every tier; a corrupted or
  substituted chunk in any tier yields `EIO` and a `FAILED` sandbox, never served
  content.
- A tampered or unsigned manifest fails the mount.
- The manifest Merkle root recorded on the sandbox matches a recomputation from
  the admitted image.
- From inside a sandbox there is no path to the `ignitionfs` socket, the node CAS
  cache, CAS credentials, or another project's manifest or chunks.
- Private-tier chunks do not dedup across projects; public base-tier chunks do.
- Private-tier chunks at rest are encrypted under a per-project key; object-read
  and decrypt grants are separate.
- Daemon kill during a running sandbox is recovered by remount from the manifest
  with no loss beyond the killed process; chunk cache survives the restart.
- Chunk fetch failure after retries fails the affected sandbox with
  `IMAGE_UNAVAILABLE` and releases quota, without affecting other sandboxes on the
  node.
- `ignitionfs`-lazy is benchmarked against eStargz/SOCI for each representative
  workload class, and selected only where it wins or is intentionally forced.

## Rollout

1. Ingest, CAS, and manifest only; no runtime integration. Measure dedup ratio,
   chunk count, and storage on a corpus of real customer images.
2. `ignitionfs` read path benchmarked standalone (`fio`, then a representative
   model load) against local NVMe and against SOCI.
3. Wire into `runsc` on the custom fleet behind a per-image selection flag;
   A/B cold-start latency against SOCI.
4. Prefetch profiles and warm-node chunk pinning.
5. Per-zone read-through cache, if cross-node first-fetch cost justifies it.
6. Per-project keyed and encrypted tier for private content.
