# Ignition productionization and commercialization proposal

Date: 2026-09-11. Scope: current working-tree code, selected tests, deployment manifests, and billing-provider documentation. This is a proposal, not an implementation or a certification of the deployed service. Existing uncommitted changes were included in the review and left intact. No live cloud resources were inspected or changed.

## Recommended first product

Sell an operated sandbox platform deployed into one customer's Google Cloud account, using GKE Standard and Cloud SQL. Deploy API, controller, gateway, database, and execution pools together initially; the current controller directly accesses the product database and Kubernetes. A central SaaS control plane managing remote customer clusters would require additional connectivity, identity, and remote-controller protocols.

Initial buyer: the platform lead or CTO at a company already running coding agents or evaluations on GCP. Initial job: launch isolated, ephemeral Linux environments, run commands, collect results, and clean up resources reliably. Start with CPU workloads; offer dedicated L4 environments only when a customer needs and funds that capacity.

Proposed sales sentence: "Ignition operates the execution environments for your AI agents inside your Google Cloud account, with project access controls, resource limits, and a programmable lifecycle."

Do not promise arbitrary unmodified Docker images with full exec support, durable sessions, millisecond starts, private-service access, unlimited scale, or GPU sharing. Managed images currently require `/ignition/init`; native-entrypoint mode loses exec and idle tracking; the root filesystem is read-only; scratch is ephemeral. Default network policy blocks private-address access even when public internet access is enabled.

## Sales and pricing experiment

1. Recruit 10–15 teams already operating agent execution infrastructure. Ask to see a representative workload, failure history, infrastructure bill, and maintenance effort. Identify the budget owner and the deployment/security approver.
2. Select 2–3 design partners with the same environment requirements. Offer a four-week paid pilot in one GCP account and region, with fixed resource and support limits.
3. Test a $2,000 fixed pilot fee, cloud infrastructure paid directly by the customer. This is a proposed price experiment, not a measured willingness to pay. Scope onboarding effort; quote unusually complex integrations separately.
4. Agree on success measures before installation: time to first real workload, startup and exec latency, successful completion rate, cleanup delay, operator hours, and cloud cost per completed task.
5. After success, test $2,000–$5,000/month platform subscriptions with included capacity bands, explicit support hours, and separately quoted additional installations. Validate pricing against demonstrated value and your delivery costs.

Maintain a small set of repeatable deployment profiles. If each sale requires bespoke platform engineering, revise the product scope before expanding sales. Publish measured case studies only with customer approval. Grow within accounts through additional teams and capacity. Defer an open anonymous trial until abuse controls and reliable spending limits exist.

Track software revenue separately from customer-paid GCP spend. At $3,000/month, 30 customers would mean $1.08M annual recurring software revenue; that is arithmetic, not a market forecast. Contribution margin must subtract installation amortization, ongoing support/on-call effort, and any vendor-hosted infrastructure.

## Existing foundations

- API/controller separation, project RBAC, OIDC, transactional admission and idempotency: `internal/api`, `internal/auth`, `internal/store/postgres_sandbox.go`.
- GKE Sandbox profile with non-root execution, no service-account token, dropped capabilities, read-only root, and runtime deadlines: `internal/k8s/spec.go`.
- Default-deny sandbox networking and separate internet-enabled pools: `deploy/k8s/base/sandbox-network-policies.yaml`, `internal/k8s/profile.go`.
- Image digest resolution and SSRF protections: `internal/imagecatalog/remote.go`, `guard.go`. Registry authorization still needs finer customer/project scope when sharing credentials.
- Controller metrics/admin health and signal cancellation already exist: `internal/controller/controller.go`. Deployment probes are a separate missing integration.
- Cloud SQL backup/PITR configuration, replica deployments, and disruption budgets exist. Restore, failover, and workload durability are different requirements and need operational verification.

Some status documents are stale: digest-based catalog resolution and controller health/shutdown are already in code. Reconcile documentation with implementation before using it in customer commitments.

## Prioritized engineering gaps

### P0: correctness and containment before a production pilot

**Controller leadership and stale writes.** `internal/controller/controller.go` defaults to a 10-second lease and rechecks it after 50 sandboxes. Kubernetes calls can take 15 seconds (`internal/k8s/cluster.go`), and supervisor probes can take three seconds each. A slow pass can exceed the lease before renewal. `UpdateObserved` rejects terminal-row updates but does not validate other transitions against current state; a stale READY observation can overwrite TERMINATING. This is a code-path risk, not a reproduced incident.

Implement time-based independent renewal, cancellation on loss, controller-epoch checks in database writes, and version/expected-state guards. Pass cancellation through Kubernetes operations; use Kubernetes UID/resource-version preconditions where applicable. Database fencing alone cannot atomically fence Kubernetes side effects, so design repeat-safe operations and reconcile them. Test two controllers, lease expiry during a blocked call, and terminate racing READY; no state resurrection or duplicate quota release should occur.

**Quota versus resource release.** `TerminateSandbox` in `internal/store/postgres_sandbox.go` releases active quota when termination is requested. Pods can still consume resources. Controller-driven idle termination records FINISHED after a delete request succeeds, before confirmed absence. Separate admission/resource reservations from user-visible lifecycle and billing boundaries. Count terminating allocations until release is confirmed; reconcile reservations independently. Test rapid create/terminate churn, failed deletion, and node loss.

**Durable cleanup.** `internal/store/postgres_controller.go` only includes terminal rows for 15 minutes. A cleanup failure lasting longer can leave a Pod outside the terminal-row scan. Introduce durable cleanup state and a periodic inventory reconciliation of owned Pods versus sandbox records. Use exact ownership and Pod UID checks. Do not treat a missing row after a database restore as immediate authorization to delete arbitrary resources. Test outages longer than 15 minutes and restore recovery.

**Bounded command transport.** `syncProcesses` in `internal/controller/reconcile.go` serializes all process records, including historical commands and environments, into a Pod annotation every pass. `CreateProcess` has no per-sandbox count limit. This creates growing control-plane payloads and can hit Kubernetes metadata limits. Short term: enforce count/byte/concurrency limits, prune completed desired entries only after acknowledgement, and avoid unchanged patches. Preserve execution tombstones so pruning/replay cannot rerun completed commands. Measure downward-API propagation latency. For interactive workloads, replace annotation command delivery with an authenticated, acknowledged channel; do not put platform credentials in tenant processes.

**Image and credential boundaries.** Require pinned images for production; remove the controller's mutable fallback on catalog failure. `RemoteResolver` uses ambient platform credentials, while the allowlist scopes registry hosts rather than repository ownership. For shared installations, authorize repository paths per project before resolving or running images. Secret values are injected into Pod environments and process environments are stored in SQL/annotations; define who can read those surfaces, redact support exports, and test cross-project access. A secret intentionally given to tenant code is readable by that code.

**Abuse and resource bounds.** Add per-project CPU, RAM, GPU, active/terminating sandbox, process, stream, and request limits. The current quota is primarily an active-sandbox count with a deployment-wide configured maximum. Add bounded stream buffers and test slow readers, output floods, process floods, disk exhaustion, and supervisor disruption. Enforce spending controls from trusted control-plane data; tenant-reported idle time is not a billing authority.

**Deployment completeness.** The checked-in prod overlay does not include the GPU-agent or prober components, and uses mutable `prod` image tags. Include the GPU agent whenever GPU capacity is offered; otherwise reject GPU requests. Render and validate a complete customer overlay, pin release digests, connect controller health endpoints to probes, and run authenticated create/exec/terminate checks during promotion. API liveness and readiness currently both use the database-dependent `/healthz`; separate process liveness from dependency readiness to avoid database outages triggering restart storms.

### P1: repeatable paid operations

- Versioned SQL migrations and a predeploy migration job. API startup currently applies a baseline schema; `CREATE TABLE IF NOT EXISTS` does not evolve existing tables. Remove runtime DDL privileges. Test upgrades from the previous release and backwards-compatible rollback.
- Customer/project provisioning, organization-to-project ownership, service identities, revocation, and documented offboarding. Current routes lack project and secret management. An audited operator CLI is sufficient for initial pilots; a complete web console is not required.
- Standard managed base images and an image-building guide. Make native-entrypoint limitations explicit and test successful native-process completion separately; the reconciler lacks an explicit Pod Succeeded branch.
- Dataset/artifact delivery using tightly scoped object access, retention, and export. Offer ephemeral jobs first. Add private-service connectivity only with explicit destination policy and real-network tests.
- Durable customer audit records for authorization, launches, exec actions, termination, configuration changes, and administrative support access; redact payloads. Existing RBAC log lines are only part of this requirement.
- Per-customer dashboards, alert routing, runbooks, a support process, and a vulnerability-update cadence. Measure warm and cold starts separately, using realistic images and repeated exec calls.
- Restore and upgrade drills. Database recovery does not recover lost `/scratch` data. Document job retry and idempotency expectations and measure achievable recovery objectives before contracting them.
- Retention/archival for process history, operations, and idempotency records. The current idempotency table has no expiry timestamp; choose and implement an explicit replay-retention contract.

### P2: expand only after measured need

Work queues/informers and bounded parallel reconciliation; controller sharding; additional regions/clouds; persistent volumes or snapshots; fine-grained egress proxying; enterprise federation; a self-service console. Benchmark the existing controller before promising high concurrency. A new hypervisor or custom fleet manager is not required for the first paid product.

## Billing design

### Commercial policy

Start with a fixed subscription/invoice for each contracted deployment and agreed capacity band. GCP bills the customer directly. Collect shadow usage from day one, but do not issue usage overages until reconciliation and customer-visible reports are reliable.

Later offer either a BYOC platform fee plus measured overages, or a separate hosted compute product. Keep SKU definitions explicit: CPU millicore-seconds, memory MiB-seconds, and dedicated L4 sandbox-seconds. A whole-L4 SKU must account for its dedicated node; state whether host CPU/RAM is bundled to prevent double charging. Warm reservations require an explicit reserved-capacity charge or must be included in the platform fee.

For hosted execution, the proposed billable interval starts when the service first declares the sandbox usable and ends at the earliest persisted accepted termination request, trusted workload completion, or service-detected loss of usability. Customer code errors still consume billable runtime; provision failures before READY do not. Idle READY time is billed until termination because automatic pause is not implemented. Charge no customer runtime for platform cleanup after accepted termination. Track allocation-to-release time separately for your actual infrastructure costs.

Persist `termination_requested_at`, workload stop/observation timestamps, and resource-release confirmation rather than deriving all charges from `finish_time - ready_time`. Store observation uncertainty; controller outages must not silently turn into unbounded charges. Reconcile affected sessions and use an explicit adjustment policy. Native jobs can complete before controller READY observation; exclude that mode from metered service initially or establish authoritative runtime timestamps first.

### Data model and flow

```text
API/controller lifecycle transaction
  -> lifecycle event + usage session + transactional outbox
  -> metering worker: close non-overlapping usage intervals
  -> immutable usage ledger + versioned pricing / customer usage report
  -> delivery worker -> Stripe Billing Meters / invoices
  -> verified webhook inbox -> local account entitlement state

Periodic reconciliation: database sessions <-> owned workloads <-> ledger <-> invoice
```

Proposed tables:

| Table | Purpose |
|---|---|
| `billing_accounts`, `billing_account_projects` | Buyer, currency, Stripe customer, effective project ownership |
| `contracts`, `price_versions`, `entitlements` | Subscription, included usage, immutable rates, quotas, credit policy |
| `lifecycle_events` | Append-only event ID, sandbox/generation/Pod UID, timestamp, source, reason |
| `usage_sessions` | Resource snapshot, billable start/stop, allocation/release, last metered boundary |
| `usage_ledger` | Non-overlapping intervals, integer quantities, account, SKU, price version, corrections |
| `billing_outbox`, `billing_deliveries` | Persistent event delivery and acknowledgement state |
| `webhook_inbox` | Unique provider event IDs and processing status |
| `invoice_reconciliations` | Period totals, pending delivery, discrepancies, approval and adjustments |

Use unique event and interval keys, for example `(sandbox_id, generation, meter, interval_start, interval_end)`, plus serialized session advancement to prevent overlaps. A unique key alone does not prevent different overlapping intervals. Write ledger increments, metering cursor advancement, and delivery outbox rows in one transaction. Split intervals at billing-period, price, and ownership boundaries. Retain fractional units internally; round according to the published invoice policy, not at every worker tick. Use integers/fixed precision for quantities and money.

Create lifecycle events in the same transaction as state changes in `postgres_sandbox.go` and `postgres_controller.go`. Add `internal/billing` and an `ignition-metering` worker rather than network calls to Stripe inside CreateSandbox. Initial fixed-price pilots can use manually reviewed Stripe invoices while this is built.

For customer-hosted deployments, use an authenticated per-installation usage exporter with durable local spooling and central deduplication. Export only IDs, resource quantities, and timestamps required for billing. Customer administrators control their infrastructure, so signing with a key stored there does not make usage tamper-proof. Fixed committed contracts and agreed audit/reporting terms are the pragmatic initial approach; restricted/offline installations can use reviewed usage exports.

### Stripe integration and operational rules

Use Stripe Billing Meters for new usage-based integration, plus subscriptions, hosted payment collection/customer portal, and invoices. Stripe processes meter events asynchronously; local admission and budgets must not depend on its usage-summary freshness. [Stripe usage documentation](https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage)

Send stable identifiers and retain durable local deduplication. Provider deduplication windows are finite: uncertain deliveries outside the supported window require reconciliation rather than blind replay. Pin and test the API version and its accepted event-time/backfill/correction constraints. [Stripe meter event implementation reference](https://github.com/stripe/stripe-go/blob/master/v2billing_meterevent.go)

Verify webhook signatures over the raw body; durably enqueue before acknowledging; tolerate duplicates and reordered events; reconcile account state against authoritative provider objects. Payment failure enters a defined grace policy and can block new launches. Keep termination and usage export available. Do not destroy workloads on one failed webhook. [Stripe webhook documentation](https://docs.stripe.com/webhooks)

For hosted trials/prepay, reserve estimated maximum liability transactionally before admission; settle actual usage and release unused reservations. A truly strict cap needs reservation or enforceable runtime deadlines, because periodic metering always lags. For BYOC, describe any displayed cloud-spend estimate as an estimate; it cannot cap unrelated customer GCP charges.

Required billing tests: replayed creation; duplicate/reordered lifecycle events; two metering workers; long sessions crossing month-end; termination before READY; controller/database outage; node disappearance; Stripe timeout after acceptance; duplicate/reordered webhooks; corrections; pricing changes; cancelled subscriptions; account moves; exporter outage; and ledger-to-invoice reconciliation. Reprocessing the same input must produce the same payable total.

## Delivery sequence and acceptance gates

Estimates assume two experienced engineers and one cloud/runtime scope. They are planning ranges, not commitments; founder sales runs in parallel.

| Phase | Approximate duration | Exit evidence |
|---|---|---|
| 1. Bound the product and fix correctness | Weeks 1–2 | One chosen buyer/workload; deployment profile; lifecycle race, lease, quota, and cleanup regressions covered; process byte/count limits enforced |
| 2. Repeatable deployment and pilot | Weeks 3–4 | Fresh customer installation; migration/rollback; authenticated lifecycle and exec checks; isolation tests; scoped paid pilot |
| 3. Operational proof and metering | Weeks 5–6 | Load and fault tests at contracted capacity; restore drill; alert delivery; durable usage sessions/ledger; customer-visible shadow bill |
| 4. Billing automation and expansion | Weeks 7–8 | Stripe test-mode failure suite; invoice reconciliation; payment/grace rules; first reconciled billing period; second installation without bespoke code |

A production pilot requires P0 fixes, a declared supported capacity, demonstrated cleanup and cross-project isolation, a tested rollback, and a named incident owner. Broad availability additionally requires measured service objectives, at least one complete reconciled billing period, repeatable installation/upgrade, and tested recovery under realistic workload volume.

Suggested performance test matrix: warm/cold CPU launches, optional L4 launches, small/large images, repeated short exec calls, 10/50/100 concurrent sandboxes, slow/unreachable supervisors, controller handoff, database outage, and node loss. Select a commercial limit from results rather than treating the configured limit of 100 as a verified capacity claim.

## Verification performed for this review

Passed the existing tests for `./internal/api`, `./internal/controller`, `./internal/k8s`, `./internal/imagecatalog`, `./internal/auth`, `./internal/gateway`, `./internal/store` (including its PostgreSQL test-container setup), and `./tests/integration/...`. `git diff --check` passed for tracked working-tree changes. No new regression tests or runtime changes were made. The listed race/scale/failure findings come from static code inspection; they were not reproduced against a live GKE deployment. Existing test success does not establish load capacity, security certification, or production readiness.
