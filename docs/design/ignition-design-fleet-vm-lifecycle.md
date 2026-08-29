# Ignition Fleet and VM Lifecycle Design

**Status:** Draft v0.1  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines immutable worker images, GCE Managed Instance Groups, warm GPU capacity, Spot/on-demand policy, registration, maintenance, rollout, rollback, and cost controls.

## Topology

- A regional CPU-only GKE cluster hosts control-plane services on nodes spread across at least three zones, with topology spread and one-zone spare capacity.
- GPU workers run in GCE MIGs, not GKE GPU node pools.
- Pools are shared platform capacity. A pool key is `(gcp_project, region, zone, machine_type, provisioning_model, compatibility_tuple)` and never contains a customer organization or project.
- On-demand and Spot use separate MIGs.
- Initial untrusted-customer pools expose one GPU per worker.
- GCE autoscaling and proactive instance redistribution are disabled. `ignition-fleet` is the sole writer of MIG target size.

## `ignition-fleet`

Owns:

- desired MIG sizes;
- warm-buffer policy;
- instance-template generations;
- blue/green rollout;
- quota and stock monitoring;
- draining obsolete pools.

It does not place sandboxes or mark a worker schedulable.

## Worker image

Base: minimal Ubuntu 24.04 LTS.

Bake:

- exact NVIDIA driver;
- `runsc` plus containerd content/snapshot services; the task API and `containerd-shim-runsc-v1` are excluded from the initial lifecycle;
- containerd;
- selected lazy snapshotter;
- `cuda-checkpoint`;
- DCGM;
- `ignitiond`, `ignition-hostd`, `snapshotd`, and `ignition-ingress`;
- telemetry agent;
- compatibility-tuple probe.

Build with Packer or Cloud Build, verify provenance/checksums, test on a GPU canary, and publish an immutable image digest. Never upgrade live workers in place.

## Worker lifecycle

```text
PROVISIONING
→ BOOTING
→ REGISTERING
→ BURN_IN
→ READY
→ DRAINING
→ TERMINATING
```

Any severe boot, tuple, driver, GPU, or health failure enters `QUARANTINED` and causes VM replacement.

MIG health checks only VM/process liveness. Scheduler readiness additionally requires worker registration, exact tuple, GPU burn-in, time synchronization, storage access, and control-stream health.

## Warm capacity

Maintain workers that are booted, driver-loaded, healthy, registered, and idle.

Initial policy:

```text
desired = active + max(min_buffer, ceil(arrival_rate * p95_provision_seconds))
```

Clamp by quota, budget, maximum buffer, and observed GPU availability. Refill asynchronously after placement. A snapshot does not replace warm VM capacity because VM allocation and driver startup remain outside sandbox restore.

## Scale-in

Scale-in never blindly lowers a regional or zonal MIG target. `ignition-fleet` removes one concrete worker at a time:

1. select a specific idle worker by stable worker ID and managed-instance URL;
2. atomically cordon it in Postgres and record a drain operation;
3. wait for scheduler exclusion and worker drain acknowledgement;
4. verify through a locked database read that no active, releasing, or suspect lease references the worker;
5. query the worker under the current owner epoch and verify no local allocation or lease and that process, device FD, mount, and cgroup cleanup is complete;
6. delete that exact managed instance through the MIG API and wait for confirmed deletion;
7. reconcile the MIG target only after deletion, without permitting automatic replacement.

Any stream loss, acknowledgement mismatch, database/local disagreement, or health failure cancels ordinary scale-in, marks the worker `SUSPECT`, and invokes forced VM recreation before its GPU can re-enter service. Concurrent scale actions serialize on the worker row and managed-instance identity.

## Spot policy

- Spot provides overflow and cost optimization, not all latency-critical baseline capacity.
- Keep distinct reliability classes for on-demand and Spot.
- Do not create periodic or preemption-time runtime recovery snapshots in initial production.
- Fail or retry in-flight work and recreate the sandbox from its immutable golden startup snapshot.
- Correctness and durability do not depend on GCP's extended preemption-notice preview; observe it only as an optional drain optimization.

## Maintenance

On maintenance notice:

1. mark worker draining;
2. stop placement;
3. drain requests;
4. fail or retry requests that cannot complete by the drain deadline;
5. recreate replacements from the immutable golden startup snapshot;
6. terminate before deadline.

Planned maintenance and Spot preemption remain separate workflows.

## Rollout

1. build new compatibility tuple;
2. create canary instance template and MIG;
3. run runtime/GPU/checkpoint qualification;
4. admit internal canary traffic;
5. increase green capacity;
6. drain blue workers;
7. retain rollback capacity;
8. delete blue only after soak.

Never restore snapshots across tuple generations without explicit certification.

## Security

- Shielded VM, vTPM, integrity monitoring, and Secure Boot when signed driver modules permit.
- No public worker IPs.
- IAP/OS Login for audited break-glass access.
- Dedicated narrow service account per pool.
- Immutable root image and minimal packages.
- No unrelated workloads on GPU workers.
- Worker artifact reads use operation-scoped signed URLs or downscoped credentials restricted to exact object paths, methods, and short expiry. The worker service account has no broad customer-project artifact access and no Secret Manager accessor role.

## Observability

Measure provisioning latency, stock failures, quota, worker-state duration, warm buffer, idle cost, image rollout, tuple mismatch, burn-in failure, interruption rate, and replacement time.

## Acceptance tests

1. **Pool isolation:** create demand from two customer organizations and three customer projects for the same platform tuple; assert all demand maps to one shared pool key containing only GCP project, region, zone, machine type, provisioning model, and tuple.
2. **Sole scaler:** inspect every production MIG and deployment policy; assert GCE autoscaling and proactive redistribution are disabled and only the `ignition-fleet` identity can update target size or delete managed instances.
3. **Specific scale-in:** seed three idle workers, select worker `B`, and trigger one scale-in; assert `B` is cordoned/drained and its exact managed-instance URL is deleted while `A` and `C` remain unchanged.
4. **Scale-in race:** create a lease after candidate selection but before the locked verification; assert deletion is cancelled. Repeat with a worker-local lease absent from Postgres; assert `SUSPECT`, forced recreation, and no in-place GPU reuse.
5. **No blind target decrement:** make the selected managed instance disappear or change identity before deletion; assert fleet does not lower target or delete a substitute instance and reconciliation raises an alert.
6. **Registration gate:** create a MIG worker and withhold tuple match, burn-in, time sync, storage access, and control-stream health in turn; assert the scheduler never sees `READY` until all checks pass.
7. **Zone loss:** remove one GKE/GPU zone at launch load; assert control services remain spread and available, surviving pools retain spare capacity, and fleet creates only explicitly selected replacement instances in allowed zones.
8. **Artifact scope:** give a worker an operation credential for one object; assert allowed read succeeds while sibling objects, another customer project, write/delete, and Secret Manager access are denied; assert expiry revokes the allowed read.
9. **Quarantine and rollout:** inject GPU health failure during blue/green rollout; assert the VM is recreated, the same VM never returns to service, active traffic stays on certified tuples, and rollback retains compatible capacity.
