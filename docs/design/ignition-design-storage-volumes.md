# Ignition Storage Design

**Status:** Draft v0.2  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope and explicit exclusions

Initial production storage is limited to immutable images, per-sandbox ephemeral scratch, authorized read-only dataset/artifact mounts, and content caches.

Initial production does **not** offer:

- a persistent writable `Volume` resource or writable persistent mount;
- a public SESSION memory-snapshot/checkpoint resource or API;
- restore, clone, migrate, durability, writer fencing, detach/flush, or crash-consistency promises for either feature.

Persistent writable volumes and public SESSION memory snapshots are post-v1 design work. They require an explicitly selected backend and defined regional placement, durability, consistency, fencing, backup/restore, encryption, quota, billing, and lifecycle semantics before any public resource, endpoint, SDK handle, or permission is introduced. Internal runtime checkpointing, if used for platform recovery, is not tenant-visible and creates no public persistence guarantee.

## Initial storage classes

- **Rootfs image:** immutable OCI manifest and content-addressed layers. A sandbox references a resolved image digest, never a mutable tag.
- **Scratch:** per-sandbox-generation writable filesystem on worker-local storage, mounted at a declared sandbox path, quota-bounded, and deleted on normal cleanup.
- **Dataset/artifact mount:** immutable or version-pinned project-authorized content exposed read-only to a sandbox.
- **Content cache:** disposable page, Local-SSD, or zonal content-addressed cache for verified image/dataset objects.

Caches are never authoritative. Scratch is not persistent storage.

## Image and read-only object contract

Image imports resolve all tags to immutable digests. Catalog state becomes `READY` only after the complete manifest, every referenced object, media-type/size constraints, malware/policy checks, and content digests are verified. Temporary import prefixes cannot be referenced by a ready catalog row.

Dataset/artifact mounts identify a project-authorized immutable version and authenticated manifest. Worker credentials are short-lived and bound to project, sandbox generation, manifest, and read-only operation. A trusted worker service fetches and verifies content, then presents it to the sandbox as read-only; tenant input never becomes a host path. Attempts to remount writable, overlay an undeclared host path, or cross into another mount fail closed.

Object deletion is asynchronous and tombstoned. Garbage collection removes content only after no ready catalog reference, active sandbox lease, or safety-window reference remains.

## Scratch lifecycle

Scratch belongs to exactly one `(project_id, sandbox_id, generation)` and is never attached to another generation. It has byte and inode quotas reserved before sandbox start. Exhaustion returns an explicit storage error and cannot consume worker-reserved capacity.

Normal termination unmounts scratch and securely schedules cleanup. Worker process, node, Local SSD, zone, or unrecoverable filesystem loss destroys scratch. The API reports sandbox failure but does not attempt transparent recovery from stale data.

**Scratch data is lost on worker loss.** This is part of the public contract and SDK/CLI documentation. Sandbox rescheduling starts with empty scratch.

## Encryption and credentials

- Provider encryption covers object storage and worker disks.
- Project/domain data keys for authoritative immutable objects are wrapped by Cloud KMS.
- Object-read and key-decrypt permissions are separate.
- Worker object credentials are short-lived and bound to immutable manifest and operation.
- KMS or credential validation failure fails closed.

Scratch relies on encrypted worker-local storage but has no backup or recovery copy. Sensitive long-lived data should not be placed in scratch.

## Quotas and cleanup

Enforce project and sandbox limits for:

- image and dataset/artifact bytes and object-store operations;
- scratch bytes and inodes;
- cache disk reservation and GC backlog.

Reserve quota before large imports and reconcile it against verified usage. Unreachable temporary import data is garbage-collected after a safety window. Worker admission preserves capacity for cleanup even when tenant scratch is full.

## Failure handling

- Worker or Local-SSD loss: scratch is lost; sandbox rescheduling receives empty scratch.
- Image/dataset cache corruption: discard and refetch from the authoritative immutable object after digest verification.
- Object store unavailable: new image/dataset reads fail retryably; already verified cached content may be used while its authorization lease is valid.
- KMS unavailable or wrong-project credentials: fail closed.
- Cleanup failure: quarantine the path/generation from reuse and retry cleanup; never attach it to another tenant or generation.

## Observability

Measure image import bytes/status/latency, immutable-object verification failures, scratch bytes/inodes and loss events, read-only violations, quota denials, cache hits/corruption, temporary-data age, and GC backlog. Do not record file contents or raw sensitive paths.

## Acceptance

- Public OpenAPI, SDK, CLI, permissions, and resource schemas contain no Volume or SESSION snapshot resource/endpoint/handle in initial production.
- Documentation and failure-injection tests demonstrate that worker loss destroys scratch and reschedules with empty scratch.
- Dataset/artifact mounts remain read-only under direct writes, remount attempts, and sandbox privilege escalation.
- No temporary import becomes visible as a ready catalog reference.
- Cache loss/corruption preserves correctness because every authoritative object is immutable and digest-verified.
- Encryption, expired credentials, wrong-project object/key access, and KMS failure all fail closed.
- GC and quarantine tests prove abandoned data is removed and a failed-cleanup scratch generation is never reused.
