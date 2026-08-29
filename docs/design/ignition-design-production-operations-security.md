# Ignition Production Operations and Security Design

**Status:** Draft v0.1  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines threat ownership, service identity, IAM, secrets, audit, observability, metering, SLOs, incident response, disaster recovery, release security, and operational gates.

## Threat model

Adversaries include:

- untrusted customer code with sandbox root;
- malicious images and commands;
- compromised client credentials;
- cross-organization and cross-customer-project resource-ID probing;
- malformed GPU/runtime operations;
- denial-of-service and quota abuse;
- compromised or stale worker process.

Trusted:

- GCP physical platform;
- worker VM kernel;
- pinned gVisor and NVIDIA driver;
- Ignition control/worker services;
- configured identity provider.

Host-administrator and GCP-operator confidentiality is outside the initial guarantee.

## Service identity

- GKE services use GKE Workload Identity Federation solely to obtain Google API credentials; static service-account keys are prohibited.
- SPIFFE/SPIRE provides workload identity and mTLS for all internal RPC. SPIRE uses `k8s_psat` attestation on GKE and `gcp_iit` attestation on GCE.
- SPIFFE IDs identify environment, service, workload class, and, for workers, expected GCP project and pool.
- Worker identity is attested to the expected GCP project, MIG/template, and service account.
- Separate development, staging, and production trust domains.
- Preview GCP Managed Workload Identities are not an initial-production dependency. Google API authentication and internal mTLS remain separate mechanisms.

## IAM

Each service receives only required permissions:

- API: resource metadata, no VM mutation.
- Scheduler: leases and desired state, no cloud provisioning.
- Fleet: MIG/instance-template mutation, no customer secrets.
- Builder: image read and artifact publish.
- Worker: operation-scoped artifact access and telemetry.
- Gateway: route resolution, no snapshot decryption.

Use separate database roles and GCP service accounts. Review effective permissions continuously.

## Secrets

- Store secrets in Secret Manager.
- Return only secret IDs and metadata to clients.
- Resolve secret values in an authorized control service and deliver them to the assigned sandbox over the current SPIFFE-authenticated stream; worker service accounts have no Secret Manager accessor role.
- Inject without command-line arguments or logs.
- Rotate and revoke by version.
- Snapshot policy either excludes secret material or explicitly encrypts and documents its presence.

## Audit

Record actor, organization, customer project, action, resource, result, policy revision, request ID, source, and timestamp for:

- authentication and authorization decisions;
- sandbox create/exec/terminate metadata;
- secret use;
- snapshot create/restore/delete;
- GPU lease;
- worker/fleet administration;
- policy and IAM changes.

Do not record command payloads, environment values, stdin/stdout, model inputs, outputs, or snapshot plaintext by default.

## Observability

Golden signals:

- API/gateway latency, errors, traffic, saturation;
- scheduling queue and lease conflicts;
- worker heartbeat and reconciliation lag;
- sandbox startup/restore decomposition;
- GPU utilization, memory, XID, ECC, and health;
- snapshot throughput and failures;
- cache hit ratio and object-store traffic;
- database/outbox/Pub/Sub health;
- cost and GPU allocation utilization.

Propagate trace IDs across API, scheduler, worker-control, runtime, snapshot, and gateway.

## Metering

The append-only `usage_ledger` is authoritative for billable usage derived from leases plus worker observations:

- GPU-seconds by SKU;
- CPU/memory allocation-seconds;
- snapshot and volume bytes;
- network egress;
- optional request/process usage.

Each row has an immutable usage-event ID, organization/project, resource and lease IDs, metric, quantity, unit, database-time interval `[start_at, end_at)`, source sequence, source event ID, recorded timestamp, and optional `corrects_event_id`. Unique constraints on usage-event ID and `(source, source_event_id)` deduplicate retries. Start/end boundaries are captured from Postgres time in the same transactions that activate and finish the lease; worker clocks are observations only.

Rows are never updated or deleted. Corrections append a reversing entry linked to the incorrect event and a replacement entry, preserving the original. The metering consumer inserts its event-dedup row and usage-ledger effects in one transaction.

An hourly reconciler compares lease transitions, worker observations, usage-ledger intervals, artifact/storage measures, network export, and cloud billing data. It emits append-only corrections, records discrepancies, and pages when unreconciled quantity exceeds the launch tolerance: 0.1% of daily GPU-seconds or any single interval longer than 60 seconds.

## Initial-production SLOs

These are launch targets, not historical guarantees:

- public API availability: 99.9% monthly;
- gateway request availability: 99.9% monthly;
- scheduler decision latency: p95 at most 100 ms from successful queue claim through committed lease/assignment at launch load;
- exec attach latency: p95 at most 1 second from authenticated gateway receipt through attachment acknowledgement for a `READY` sandbox;
- golden restore: p95 application-ready at most 20 seconds for the validated L4 workload with at most 8 GiB captured VRAM and a locally cached golden artifact;
- cold lazy-image start: p95 application-ready at most 120 seconds for the validated L4 workload;
- Cloud SQL zonal failover: RPO 0 and RTO 5 minutes;
- regional disaster recovery from cross-region backup/PITR: RPO 15 minutes and RTO 4 hours.

Before launch, dashboards also segment sandbox queue/start/ready latency by cache state, exec-stream availability, restore latency by snapshot size/VRAM, Spot work loss, worker replacement time, and artifact durability. Alerts use multi-window error-budget burn rates plus correctness alerts that cannot be budgeted, including duplicate leases, cross-organization authorization failures, and usage-ledger gaps.

## Incident response

Runbooks:

- credential or signing-key compromise;
- gVisor/NVIDIA critical vulnerability;
- cross-organization isolation suspicion;
- GPU fleet XID spike;
- corrupted snapshot/image artifact;
- Postgres or regional outage;
- runaway quota/cost;
- incompatible tuple rollout.

Any isolation suspicion stops placement on affected tuple, preserves forensic metadata, revokes artifacts/routes, and escalates to security response.

## Disaster recovery

- Initial production serves from one region.
- Cloud SQL for PostgreSQL uses regional HA, private IP, the supported connector with bounded connection pools, automated cross-region backups, and PITR.
- Database clients retry a serialization failure, deadlock, or failover only by retrying the whole transaction with bounded jitter.
- Tested metadata and KMS-reference backups.
- Reproducible infrastructure and immutable images.
- Artifact retention/versioning appropriate to recovery objectives.
- Quarterly clean-environment restore.
- Quarterly restores must demonstrate the 15-minute RPO and 4-hour RTO before cross-region recovery is claimed.

## Event operations

Outbox events use immutable IDs. Dispatchers claim with `FOR UPDATE SKIP LOCKED` and a database-time lease, publish at least once, and mark published only after broker acknowledgement. Expired claims retry with bounded backoff; exhausted events retain payload and attempt history in a DLQ. Replay is authorized and audited and republishes the same event ID. Every consumer commits its dedup record in the same transaction as effects.

## Release security

- Signed source and build provenance.
- Dependency and container scanning.
- SBOM for every service and worker image.
- Secret scanning.
- Static analysis, fuzzing, race testing, and syscall/device negative tests.
- Independent threat-model review.
- Compatibility-tuple canary and rollback.
- No release with known isolation-test failure.

## Production gates

- Multi-zone services and database failover pass.
- Worker-stream ownership survives replica loss.
- Zone-loss and fleet replacement drills pass.
- Backup restore, KMS rotation, and signing-key rotation pass.
- SDK/CLI/API conformance passes.
- 1,000 sandbox lifecycle cycles leak no resource.
- Adversarial gVisor/GPU isolation suite passes.
- Sustained and burst load stay within SLO and budget.
- On-call ownership, dashboards, runbooks, and rollback exist.

## Acceptance tests

1. **Identity separation:** from a GKE pod, exchange GKE Workload Identity Federation credentials for an allowed Google API call and assert no static key exists. Establish internal RPC only with a `k8s_psat`-attested SPIFFE identity; assert Google credentials alone cannot complete mTLS.
2. **Worker attestation:** register a GCE worker with valid `gcp_iit`, then alter expected GCP project, MIG/template, and service account independently; assert each mismatch prevents SPIFFE issuance and worker registration.
3. **Authorization hierarchy:** for every public resource endpoint, test same project, different project in the same organization, and different organization; assert only explicitly authorized organization/project access succeeds and schemas/APIs use only explicit organization and project IDs for customer scope.
4. **Artifact and secret scope:** issue one operation-scoped credential; assert only the exact artifact method/path is allowed until expiry, all other customer artifacts are denied, and the worker service account cannot call Secret Manager.
5. **Metering dedup:** deliver every source event 100 times concurrently; assert one dedup row and one ledger effect. Apply a correction; assert the original is unchanged and reversing/replacement rows net to the corrected quantity.
6. **Metering boundaries and reconciliation:** skew worker clocks by ±10 minutes; assert usage intervals use database timestamps without overlap. Inject a 61-second gap and a 0.11% daily GPU-second discrepancy; assert reconciliation appends corrections and pages.
7. **Outbox DLQ replay:** force publish failures through the attempt limit, then replay; assert payload and immutable event ID are unchanged, replay is audited, and each consumer commits exactly one effect with its dedup row.
8. **Availability SLOs:** run the declared monthly-equivalent launch load and fault profile; assert measured API and gateway availability are each at least 99.9% and scheduler p95 claim-to-commit latency is at most 100 ms.
9. **Zonal failover:** force Cloud SQL primary-zone loss during multi-row transactions; assert RPO 0, recovery within 5 minutes, whole-transaction retries, and no partial or duplicate effects.
10. **Regional recovery:** restore cross-region backup and PITR into a clean region; assert the recovered point is no older than 15 minutes, service resumes within 4 hours, and resource, quota, event, and usage-ledger reconciliation passes.
11. **Security gate:** execute cross-organization isolation, payload-redaction, credential-revocation, GPU isolation, and critical-runbook drills; assert zero unauthorized access or sensitive log payloads and recorded on-call acknowledgement for every injected alert.
