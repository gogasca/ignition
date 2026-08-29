# Ignition Scheduler and GPU Leasing Design

**Status:** Draft v0.1  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines admission, queueing, placement, atomic GPU reservation, fencing, lease recovery, fairness, and capacity signals.

## Ownership

`ignition-scheduler` owns:

- placement decisions;
- queue claims and scheduling state, but not queue insertion;
- active GPU leases;
- desired worker assignment;
- compatibility matching;
- scheduling metrics.

`ignition-api` owns admission and atomically inserts the idempotency record/request hash, sandbox, operation, quota-ledger reservation, queue row, lifecycle event, and outbox event. The scheduler only acts on committed queue rows. It does not admit requests, reserve quota, provision VMs, configure GPUs, or start containers.

## Admission

Before queueing, `ignition-api`:

1. authorize project and image;
2. validate CPU, RAM, GPU, region, network, and timeout policy;
3. check project quota;
4. resolve immutable image digest;
5. validate required compatibility constraints;
6. reserves quota and writes all admission records in its single transaction.

Rejected admission never creates a GPU lease.

## Placement filters

A candidate must have:

- current heartbeat;
- worker state `READY`;
- GPU health `HEALTHY`;
- no active local or database lease;
- compatible GPU SKU;
- compatible runtime tuple for snapshot restore;
- permitted zone and provisioning model;
- sufficient CPU, RAM, disk, and network capacity.

## Placement score

Preference order:

1. already-ready sandbox reuse;
2. compatible golden snapshot in page/Local SSD cache;
3. compatible snapshot in zonal cache;
4. cached image chunks;
5. on-demand capacity for strict availability;
6. lowest predicted startup time;
7. least-recently-used device for balancing.

Scoring never weakens hard compatibility or isolation filters.

## Atomic lease

After claiming a committed queue row, the scheduler performs placement in one Postgres transaction:

1. lock the claimed queue row and candidate GPU rows with `FOR UPDATE SKIP LOCKED`;
2. revalidate worker and GPU state;
3. insert active lease under a unique GPU constraint;
4. increment the GPU fencing token;
5. assign worker and lease to sandbox;
6. mark the queue row assigned and write desired-state, lifecycle, and outbox events;
7. commit.

The scheduler database role has `SELECT/UPDATE` on queue, worker, and GPU rows; `SELECT/INSERT/UPDATE` on leases and fairness state; scoped assignment-column updates on sandboxes; and event-table insert grants. It has no insert grant on admission, idempotency, operation, or quota-ledger tables.

## Queueing and fairness

Queue by customer project and priority class. The scheduler persists per-project weight, deficit, virtual finish, and last-served sequence in `scheduler_project_state`; restart cannot reset fairness. Cost is normalized requested GPU capacity, initially one unit per GPU.

Each scheduling round uses database time and:

1. adds `quantum × project_weight` to each eligible project's deficit;
2. computes the head row's virtual finish from persistent state;
3. selects a project with sufficient deficit by `(virtual_finish, last_served_sequence, project_id)`;
4. claims that project's head queue row by `(priority_rank, enqueue_time, queue_id)`;
5. subtracts request cost and persists new deficit, virtual finish, and sequence in the same transaction as the claim.

The claim uses this SQL shape after deficits are replenished in the same transaction:

```sql
WITH project_choice AS MATERIALIZED (
  SELECT s.project_id
  FROM scheduler_project_state s
  JOIN LATERAL (
    SELECT q.cost
    FROM sandbox_queue q
    WHERE q.project_id = s.project_id AND q.state = 'QUEUED'
    ORDER BY q.priority_rank, q.enqueue_time, q.queue_id
    LIMIT 1
  ) h ON true
  WHERE s.deficit >= h.cost
  ORDER BY s.virtual_finish, s.last_served_sequence, s.project_id
  LIMIT 1
  FOR UPDATE OF s SKIP LOCKED
),
queue_choice AS MATERIALIZED (
  SELECT q.queue_id, q.project_id, q.cost
  FROM sandbox_queue q
  JOIN project_choice p USING (project_id)
  WHERE q.state = 'QUEUED'
  ORDER BY q.priority_rank, q.enqueue_time, q.queue_id
  LIMIT 1
  FOR UPDATE OF q SKIP LOCKED
),
fairness_update AS (
  UPDATE scheduler_project_state s
  SET deficit = s.deficit - q.cost,
      virtual_finish = s.virtual_finish + q.cost / s.weight,
      last_served_sequence = nextval('scheduler_service_sequence')
  FROM queue_choice q
  WHERE s.project_id = q.project_id
  RETURNING s.project_id
)
UPDATE sandbox_queue q
SET state = 'CLAIMED',
    claim_owner = $1,
    claim_deadline = transaction_timestamp() + $2::interval
FROM queue_choice c, fairness_update f
WHERE q.queue_id = c.queue_id AND f.project_id = c.project_id
RETURNING q.*;
```

All ordering columns include the unique IDs shown above, so replicas make the same logical choice before row-lock contention and skip only rows locked by another claimant. Aging may improve a queued request's priority rank but cannot bypass quota or compatibility.

Queue deadlines, claim leases, lease expiry, and aging compare against `transaction_timestamp()`/`statement_timestamp()` from Postgres, never scheduler host clocks. Enforce maximum queue age and project queued-sandbox quota; return retryable `CAPACITY_UNAVAILABLE` after the database-time deadline. Do not hold an API or database transaction while waiting. Emit queue depth and compatible-capacity demand to `ignition-fleet`.

Priority does not preempt an active untrusted customer workload in the initial release.

## Lease lifecycle

```text
RESERVED → ACTIVATING → ACTIVE → RELEASING → RELEASED
RESERVED/ACTIVATING/ACTIVE → SUSPECT
SUSPECT → RELEASED only after VM recreation and replacement-worker validation
```

Each worker persists a local high-water fencing token per physical GPU and accepts a command only when its token is at least the high-water value; a higher token is durably recorded before execution. A token at or below the high-water value cannot activate a different lease.

Normal release requires an acknowledgement from the current worker-control owner epoch and fencing token proving: sandbox and child-process exit, GPU/device FD closure, mount cleanup, cgroup cleanup, local allocation removal, and passing worker/GPU health checks. Only then may the transaction mark the lease `RELEASED` and the GPU reusable.

Lease expiry, stream disconnect, missing acknowledgement, token/epoch mismatch, unexpected local allocation, or failed cleanup marks the lease and worker `SUSPECT`. A suspect GPU is never released in place: `ignition-fleet` recreates the VM, and the replacement must register, report no local lease, complete burn-in, and pass GPU health validation before reuse.

## Failure handling

- Scheduler restart: reconstruct queue and active leases from Postgres.
- Worker heartbeat loss: stop new placement and mark leases suspect.
- Worker reconnect: compare worker generation, owner epoch, local allocation, and local high-water fencing token; any mismatch is suspect.
- Split brain: latest database fencing token wins; stale worker command is rejected.
- Transaction conflict: retry with jitter and bounded attempts.
- GPU health event: cordon resource and trigger sandbox failure/recovery workflow.

## APIs

Internal gRPC:

```text
GetPlacement
ReleaseSandbox
ReportWorkerObservation
ListPoolDemand
```

All requests carry operation ID and expected resource generation.

## Observability

Measure admission failures, queue duration, placement duration, candidate counts, filter reasons, lease conflicts, suspect leases, startup prediction error, project fairness, and unused warm GPUs.

Scheduler decision latency from successful queue claim through committed lease/assignment has an initial-production launch target of p95 100 ms at launch load.

## Acceptance tests

1. **Concurrent lease uniqueness:** run 50 scheduler replicas against 10 GPUs and 1,000 compatible queue rows; assert at most one active lease per physical GPU, exactly 10 assignments, and no sandbox has multiple assignments.
2. **Deterministic claim:** seed equal virtual finishes and enqueue times, run 100 concurrent claim rounds, and assert winners follow `(virtual_finish, last_served_sequence, project_id)` and `(priority_rank, enqueue_time, queue_id)` after accounting for locked rows.
3. **Persistent weighted fairness:** queue continuously backlogged projects with weights 1, 2, and 4 for 70,000 unit-cost decisions, restarting all schedulers every 1,000 decisions; assert service shares are within 2% of 1:2:4 and no eligible project waits more than two scheduling rounds beyond its weighted turn.
4. **Database-time deadline:** skew scheduler host clocks by ±10 minutes and expire a queue row; assert all replicas use the same Postgres deadline and return `CAPACITY_UNAVAILABLE` exactly once without a lease.
5. **Physical release:** withhold each required cleanup fact in turn—process exit, FD/device close, mounts, cgroups, local allocation, and health; assert release is denied and the GPU is not a placement candidate. Supply all facts with current token and epoch; assert release succeeds.
6. **Disconnect and mismatch:** disconnect during `RELEASING`, then separately report a stale token, stale owner epoch, and unexpected local lease; assert each case becomes `SUSPECT`, the original VM is deleted, and only a newly registered and burned-in VM returns the GPU to candidates.
7. **Local high-water token:** persist token `N`, restart the worker, and deliver commands with `N-1`, `N`, and `N+1`; assert the first two cannot activate a new lease, `N+1` is persisted before execution, and a later replay of `N` is rejected.
8. **Decision SLO:** at declared launch concurrency and database size, execute at least 10,000 successful claims; assert p95 claim-to-commit latency is at most 100 ms and report candidate count and lock-conflict rate.
