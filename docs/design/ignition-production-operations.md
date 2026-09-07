# Ignition production operations and security

**Status: PARTIAL — this is the bar for a production launch, not a description of
current behavior.**

**In place:** per-service GCP + KSA identities, least-privilege Cloud SQL roles,
private networking, Cloud SQL HA + PITR, audit-log lines on RBAC mutations, a CI
pipeline (`deploy/PIPELINE.md`), Terraform for the cluster / SQL / prober / IAP,
and a critical-user-journey prober on staging.

**Not built:** SPIFFE/SPIRE, the usage/metering ledger + reconciler, the
transactional outbox, cross-region DR drills, and the launch gates.

**Parent:** [shipped architecture](ignition-shipped-architecture.md) ·
[deferred runtime](ignition-deferred-runtime.md)

## Threat model

**Adversaries:** untrusted customer code with sandbox root; malicious images and
commands; compromised client credentials; cross-organization and
cross-customer-project resource-ID probing; malformed GPU/runtime operations;
denial-of-service and quota abuse; a compromised or stale worker process.

**Trusted:** the GCP physical platform; the worker VM kernel; pinned gVisor and
NVIDIA driver; Ignition control/worker services; the configured identity
provider. Host-administrator and GCP-operator confidentiality is outside the
guarantee.

## Service identity

- GKE services use GKE Workload Identity Federation **solely** to obtain Google
  API credentials; static service-account keys are prohibited.
- SPIFFE/SPIRE provides workload identity and mTLS for all internal RPC —
  `k8s_psat` attestation on GKE, `gcp_iit` on GCE. SPIFFE IDs identify
  environment, service, workload class, and (for workers) expected GCP project +
  pool. Worker identity is attested to the expected GCP project, MIG/template,
  and service account.
- Separate development, staging, and production trust domains.
- Google API authentication and internal mTLS stay separate mechanisms; preview
  GCP Managed Workload Identities are not a dependency.

## IAM

Each service receives only required permissions — API: resource metadata, no VM
mutation; scheduler: leases + desired state, no cloud provisioning; fleet:
MIG/instance-template mutation, no customer secrets; builder: image read +
artifact publish; worker: operation-scoped artifact access + telemetry; gateway:
route resolution, no snapshot decryption. Separate database roles and GCP service
accounts; effective permissions reviewed continuously.

## Secrets

Stored in Secret Manager; only IDs and metadata returned to clients. Values are
resolved by an authorized control service and delivered to the assigned sandbox
over the current authenticated stream (worker service accounts have no Secret
Manager accessor role), injected without command-line arguments or logs, rotated
and revoked by version. Snapshot policy either excludes secret material or
explicitly encrypts and documents its presence.

## Audit

Record actor, organization, customer project, action, resource, result, policy
revision, request ID, source, and timestamp for: authn/authz decisions; sandbox
create/exec/terminate metadata; secret use; snapshot create/restore/delete; GPU
lease; worker/fleet administration; policy and IAM changes. Do **not** record
command payloads, environment values, stdin/stdout, model inputs/outputs, or
snapshot plaintext by default.

## Observability

Golden signals: API/gateway latency, errors, traffic, saturation; scheduling
queue + lease conflicts; worker heartbeat + reconciliation lag; sandbox
startup/restore decomposition; GPU utilization / memory / XID / ECC / health;
snapshot throughput + failures; cache hit ratio + object-store traffic;
database / outbox / Pub/Sub health; cost + GPU allocation utilization. Propagate
trace IDs across API, scheduler, worker-control, runtime, snapshot, and gateway.

Before launch, dashboards also segment sandbox queue/start/ready latency by cache
state, exec-stream availability, restore latency by snapshot size/VRAM, Spot work
loss, worker replacement time, and artifact durability. Alerts use multi-window
error-budget burn rates **plus** correctness alerts that cannot be budgeted:
duplicate leases, cross-organization authorization failures, usage-ledger gaps.

## Metering

The append-only `usage_ledger` is authoritative for billable usage derived from
leases plus worker observations: GPU-seconds by SKU; CPU/memory
allocation-seconds; snapshot + volume bytes; network egress; optional
request/process usage.

Each row has an immutable usage-event ID, organization/project, resource + lease
IDs, metric, quantity, unit, database-time interval `[start_at, end_at)`, source
sequence, source event ID, recorded timestamp, and optional `corrects_event_id`.
Unique constraints on usage-event ID and `(source, source_event_id)` deduplicate
retries. Start/end boundaries come from Postgres time in the same transactions
that activate and finish the lease; worker clocks are observations only. Rows are
never updated or deleted — corrections append a reversing entry linked to the
incorrect event plus a replacement entry. The metering consumer inserts its
event-dedup row and ledger effects in one transaction.

An hourly reconciler compares lease transitions, worker observations, ledger
intervals, artifact/storage measures, network export, and cloud billing data;
emits append-only corrections; and pages when unreconciled quantity exceeds the
launch tolerance — 0.1% of daily GPU-seconds or any single interval longer than
60 seconds.

## Launch-target SLOs

Targets for a production launch, not measured guarantees:

| Target | Value |
|---|---|
| public API availability | 99.9% monthly |
| gateway request availability | 99.9% monthly |
| scheduler decision latency (queue claim → committed lease/assignment) | p95 ≤ 100 ms at launch load |
| exec attach (authenticated gateway receipt → attachment ack, `READY` sandbox) | p95 ≤ 1 s |
| golden restore, ≤ 8 GiB captured VRAM, locally cached artifact | p95 application-ready ≤ 20 s |
| cold lazy-image start, validated L4 workload | p95 application-ready ≤ 120 s |
| Cloud SQL zonal failover | RPO 0, RTO 5 min |
| regional DR from cross-region backup/PITR | RPO 15 min, RTO 4 hours |

## Incident response

Runbooks: credential or signing-key compromise; gVisor/NVIDIA critical
vulnerability; cross-organization isolation suspicion; GPU fleet XID spike;
corrupted snapshot/image artifact; Postgres or regional outage; runaway
quota/cost; incompatible tuple rollout.

Any isolation suspicion stops placement on the affected tuple, preserves forensic
metadata, revokes artifacts/routes, and escalates to security response.

## Disaster recovery

- Deployment serves from one region.
- Cloud SQL: regional HA, private IP, supported connector with bounded pools,
  automated cross-region backups, PITR.
- Clients retry a serialization failure / deadlock / failover only by retrying
  the whole transaction with bounded jitter — never a single statement into a
  partial transaction.
- Tested metadata + KMS-reference backups; reproducible infrastructure +
  immutable images; artifact retention/versioning matched to recovery objectives.
- Quarterly clean-environment restore, which must demonstrate the 15-minute RPO
  and 4-hour RTO before cross-region recovery is claimed.

## Release security

Signed source + build provenance; dependency + container scanning; SBOM per
service and worker image; secret scanning; static analysis, fuzzing, race
testing, syscall/device negative tests; independent threat-model review;
compatibility-tuple canary + rollback. **No release with a known isolation-test
failure.**

## Production gates

Multi-zone services + database failover pass; worker-stream ownership survives
replica loss; zone-loss + fleet-replacement drills pass; backup restore + KMS
rotation + signing-key rotation pass; SDK/CLI/API conformance passes; 1,000
sandbox lifecycle cycles leak no resource; the adversarial gVisor/GPU isolation
suite passes; sustained + burst load stay within SLO and budget; on-call
ownership, dashboards, runbooks, and rollback exist.

## Acceptance tests

1. **Identity separation** — exchange WIF credentials for an allowed Google API
   call with no static key; establish internal RPC only with a `k8s_psat`-attested
   SPIFFE identity; Google credentials alone cannot complete mTLS.
2. **Worker attestation** — register a GCE worker with valid `gcp_iit`, then alter
   expected GCP project / MIG-template / service account independently; each
   mismatch prevents SPIFFE issuance and registration.
3. **Authorization hierarchy** — for every public endpoint, test same project,
   different project in the same organization, different organization; only
   explicitly authorized access succeeds; schemas use only explicit organization
   + project IDs for customer scope.
4. **Artifact + secret scope** — one operation-scoped credential allows only the
   exact artifact method/path until expiry; all other customer artifacts denied;
   the worker SA cannot call Secret Manager.
5. **Metering dedup** — deliver every source event 100× concurrently → one dedup
   row, one ledger effect; a correction leaves the original unchanged and
   reversing/replacement rows net to the corrected quantity.
6. **Metering boundaries** — skew worker clocks ±10 min → intervals use database
   timestamps without overlap; a 61-second gap and a 0.11% daily GPU-second
   discrepancy → reconciliation appends corrections and pages.
7. **Outbox DLQ replay** — force publish failures through the attempt limit, then
   replay → payload + immutable event ID unchanged, replay audited, each consumer
   commits exactly one effect with its dedup row.
8. **Availability SLOs** — run the declared monthly-equivalent launch load + fault
   profile → measured API and gateway availability each ≥ 99.9%, scheduler p95
   claim-to-commit ≤ 100 ms.
9. **Zonal failover** — force Cloud SQL primary-zone loss during multi-row
   transactions → RPO 0, recovery ≤ 5 min, whole-transaction retries, no partial
   or duplicate effects.
10. **Regional recovery** — restore cross-region backup + PITR into a clean
    region → recovered point ≤ 15 min old, service ≤ 4 hours, resource / quota /
    event / ledger reconciliation passes.
11. **Security gate** — cross-organization isolation, payload-redaction,
    credential-revocation, GPU isolation, and critical-runbook drills → zero
    unauthorized access or sensitive log payloads, recorded on-call
    acknowledgement for every injected alert.
