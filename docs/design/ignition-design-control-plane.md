# Ignition Control Plane Design

**Status:** Draft v0.1  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines durable resource state, service boundaries, worker communication, operations, events, database ownership, availability, and recovery.

The GKE MVP first slice persists `projects`, `role_bindings`, `images`, `sandboxes`, `processes`, `operations`, `idempotency_keys`, `project_quota`, and `controller_leases` (see [API and Controller](ignition-design-api-controller.md) §7). Custom-runtime services (`ignition-worker-control`, and so on) remain specified below for the gated path (`quota_ledger`, outbox, scheduler queue).

## Services

- `ignition-api`: validates and authorizes public resource requests.
- `ignition-worker-control`: terminates worker mTLS streams and delivers desired state.
- `ignition-artifacts`: owns image, golden-snapshot, and read-only dataset-mount metadata. Persistent writable Volume resources are post-v1.
- `ignition-builder`: executes image conversion and golden-snapshot workflows.
- `ignition-scheduler`, `ignition-fleet`, and `ignition-gateway` have separate designs.

Services run independently on a regional, CPU-only GKE cluster with nodes in at least three zones. Required services use topology spread constraints, PodDisruptionBudgets, and enough reserved spare capacity to survive one zone loss. Each service has a distinct identity, database role, resource budget, deployment, and autoscaling policy.

## State model

The canonical customer hierarchy is `organization → project`. An organization is the billing and policy boundary; a project is the resource, quota, and fairness boundary. Every customer-owned resource has a non-null `project_id`; APIs and schemas use only explicit organization and project identifiers for customer scope.

Postgres is authoritative for:

- organizations, projects, and resource metadata;
- desired sandbox state;
- workers and observed state;
- GPU inventory and leases;
- operations and idempotency keys;
- image and snapshot catalog;
- lifecycle events and transactional outbox.

Worker-local state is an observed cache and cannot override control-plane ownership.

Minimum tables:

```text
organizations, projects, images, secrets, dataset_mounts
sandboxes, processes
workers, gpus, gpu_leases
snapshots, artifacts
operations, idempotency_keys, quota_ledger, sandbox_queue
lifecycle_events, outbox_events, outbox_delivery_attempts
```

## Database ownership and admission transaction

Tables are owned by a migration-only role; runtime roles cannot run DDL or grant privileges.

The end-to-end public request and asynchronous provisioning sequence is specified in [Create Sandbox API Specification](ignition-sandbox-create-api.md).

- `ignition_api`: `SELECT/INSERT/UPDATE` on `idempotency_keys`, `sandboxes`, `operations`, `quota_ledger`, `sandbox_queue`, `lifecycle_events`, and `outbox_events`; read-only access to authorization and catalog data.
- `ignition_scheduler`: `SELECT/UPDATE` on `sandbox_queue`, `workers`, and `gpus`; `SELECT/INSERT/UPDATE` on `gpu_leases` and scheduler fairness state; scoped `UPDATE` of sandbox assignment fields; `INSERT` on lifecycle/outbox tables.
- `ignition_worker_control`: read desired state; scoped updates to worker observations, command acknowledgements, and stream ownership; no admission or quota writes.
- `ignition_fleet`, `ignition_artifacts`, `ignition_builder`, and `ignition_gateway`: only the table and column grants required by their designs. They cannot write admission, queue, quota, or lease rows.

After authorization and validation, `ignition-api` performs admission in one serializable transaction. It atomically:

1. inserts or verifies the idempotency key and canonical request hash;
2. inserts the sandbox and operation;
3. appends a quota-ledger reservation;
4. inserts the queue row;
5. appends the lifecycle event and matching outbox event;
6. commits all rows or none.

An idempotency-key replay with the same hash returns the original operation; a different hash returns `IDEMPOTENCY_KEY_REUSED`. The API never creates a GPU lease or worker assignment. The scheduler separately claims a committed queue row and atomically creates the GPU lease and assignment as defined in the scheduler design.

## Desired-state protocol

Workers establish a bidirectional gRPC stream to `ignition-worker-control` using SPIFFE mTLS.

Worker messages:

```text
RegisterWorker
Heartbeat
InventoryUpdate
SandboxStatus
ProcessStatus
GPUHealthEvent
InterruptionEvent
```

Control messages:

```text
DesiredSandbox
StopSandbox
DrainWorker
PrepareGoldenSnapshot
RestoreGoldenSnapshot
RefreshCredentials
```

Golden-snapshot commands are accepted only for `ignition-builder` operations and contain the immutable image/startup-policy identity. Worker-control exposes no session or runtime-recovery snapshot command in initial production.

Every command and acknowledgement carries worker generation, desired-state generation, operation ID, GPU lease fencing token, and worker-control owner epoch.

## Stream ownership

Worker-control replicas lease worker-stream ownership in Postgres. Acquiring ownership increments a durable, monotonic owner epoch. A reconnect:

1. authenticates the worker SPIFFE identity;
2. verifies expected GCE instance and compatibility tuple;
3. increments stream generation and acquires a new owner epoch;
4. invalidates the previous connection;
5. sends the latest durable desired state.

The worker accepts exactly one command stream for its current owner epoch. It closes the previous stream when it accepts a higher epoch and rejects commands and acknowledgements with a lower epoch. Commands are idempotent and acknowledged with observed generation and owner epoch.

## Operations

All asynchronous mutations create an Operation:

```text
PENDING → RUNNING → SUCCEEDED
PENDING/RUNNING → FAILED
PENDING/RUNNING → CANCELLED
```

Operations retain structured progress, error code, timestamps, affected resource, and trace ID. Cancellation is best effort and never bypasses cleanup.

## Events

Write each resource mutation, lifecycle event, and outbox event in one transaction. Every event receives an immutable UUID event ID at insertion.

Dispatchers claim committed outbox rows with `FOR UPDATE SKIP LOCKED`, set a lease owner and database-time lease deadline, publish with the event ID as the message ID/attribute, then mark the row published only after broker acknowledgement. An expired claim is retryable. Bounded exponential backoff applies; after the configured attempt limit the dispatcher moves the event to a DLQ while preserving its event ID, payload, and attempt history. Authorized replay creates a new delivery attempt for the same immutable event ID and is audited.

Each consumer stores the event ID in its own dedup table in the same database transaction as the event's effects. A duplicate event commits neither duplicate effects nor a second dedup row. Delivery is at-least-once. Pub/Sub ordering is not a correctness mechanism; consumers validate resource generations against Postgres.

## Availability

- At least three replicas of required stateless services spread across three zones.
- Cloud SQL for PostgreSQL uses regional HA, private IP only, automatic backups, and PITR.
- Services connect through the supported Cloud SQL connector with bounded pools; connection budgets are enforced per service.
- A serialization failure, deadlock, or failover abort retries the whole transaction from its beginning with bounded jitter. Individual statements are never replayed into a partially completed transaction.
- PodDisruptionBudgets, topology spread, and one-zone spare capacity are required.
- Backward-compatible expand/migrate/contract schema changes.
- Readiness fails when mandatory dependencies are unavailable.
- Writes fail closed when authorization or authoritative state cannot be reached.

## Disaster recovery

- Initial production is single-region.
- Cloud SQL zonal failover launch target: RPO 0 and RTO 5 minutes.
- Cross-region backup/PITR recovery launch target: RPO 15 minutes and RTO 4 hours.
- Object-versioning or retention policy for manifests.
- Infrastructure and schemas reproducible from source.
- Quarterly restore into an isolated project.

## Security

- Separate database roles per service.
- No public access to Postgres or worker-control.
- TLS on every connection.
- Secrets referenced by ID and resolved only where required.
- Resource payloads and command output excluded from control-plane logs.
- Administrative mutations require elevated role and complete audit records.

## Observability

Measure request latency, operation duration, worker streams, reconnects, heartbeat age, outbox lag, event retries, database saturation, idempotency conflicts, and state-reconciliation lag.

Trace public request → durable operation → scheduler → worker command → observed state.

The API availability launch target is 99.9%, measured at the public service boundary. This and the recovery objectives above are initial-production launch targets, not historical guarantees.

## Acceptance tests

1. **Admission atomicity:** inject a failure after each admission write in turn; after rollback, assert zero rows for the request in all seven admission tables. Retry once without injection and assert exactly one idempotency record, sandbox, operation, quota reservation, queue row, lifecycle event, and outbox event.
2. **Idempotency hash:** submit the same organization/project, key, and canonical request 100 times concurrently; assert one sandbox/operation and identical responses. Change one request field with the same key; assert `IDEMPOTENCY_KEY_REUSED` and no new rows.
3. **Grant boundary:** using every runtime database role, attempt admission-table, lease-table, and DDL writes; assert only the documented owner operations succeed and all cross-owner writes fail with permission denied.
4. **Outbox crash/replay:** crash a dispatcher before publish, after publish, and before marking published; expire claims using database time and replay the DLQ event. Assert eventual delivery, one immutable event ID, and exactly one committed consumer effect.
5. **Owner epoch:** connect two worker-control replicas for one worker; after the second acquires epoch `N+1`, send a command from epoch `N`. Assert the worker has one accepted stream, rejects the stale command, and acknowledges only `N+1`.
6. **Zone loss:** remove one GKE zone at launch load; assert required services remain available, topology constraints hold, and API availability remains within the 99.9% launch-target error budget.
7. **Cloud SQL failover:** force a zonal failover during admission and outbox transactions; assert aborted work retries from transaction start, no partial/duplicate rows exist, RPO is 0, and service recovers within 5 minutes.
8. **Regional restore:** restore the latest cross-region backup plus WAL/PITR into an isolated region; assert organizations, projects, resources, operations, quota ledger, and event history reconcile to a recovery point no older than 15 minutes and service is restored within 4 hours.
