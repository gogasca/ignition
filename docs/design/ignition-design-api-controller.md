# Ignition API and Controller Technical Design Proposal

**Status:** Current — describes the deployed `ignition-api` and `ignition-controller` binaries.  
**Parent:** [GKE Sandbox](ignition-design-gke-sandbox.md), [Technical design](ignition-technical-design.md)  
**Public contract:** [Create Sandbox API](ignition-sandbox-create-api.md), [Client API and Identity](ignition-design-client-api-identity.md)  
**Schema:** [`api/proto/ignition/v1/`](../../api/proto/ignition/v1/)  
**Build and deploy:** [Implementation guide](../guides/ignition-implementation.md)

## 1. Purpose

This is the software design for the two control-plane binaries that run on GKE:

- **`ignition-api`** — public HTTP/JSON API. Authenticates, authorizes, admits work, and is the only writer of *desired* product state.
- **`ignition-controller`** — internal reconciler. Reads desired state from Cloud SQL, is the only process with Kubernetes Pod RBAC, and writes *observed* public sandbox states.

Together they implement sandbox lifecycle, process metadata, and operations on GKE Sandbox (gVisor / `nvproxy`) without the custom GCE MIG worker path.

`ignition-gateway` (exec byte stream) is specified in [Data Plane and Networking](ignition-design-data-plane-networking.md). This document covers only the attach-token minting that `ignition-api` performs.

## 2. Scope and non-goals

**In scope**

- `SandboxService`: create, get, list, terminate, watch, plus process create/get/list/attach/signal/cancel.
- `OperationService`: get, list, watch, cancel.
- Cloud SQL admission, idempotency, quota reservation, and project-scoped RBAC.
- Controller Pod create/delete, state mapping, warm balloon Pods, node quarantine on ambiguous GPU cleanup.
- Go 1.26 services; protobuf as the schema; public transport HTTP/JSON (SSE for watch).

**Out of scope**

- Custom `ignition-scheduler` / `ignition-fleet` / `ignitiond` / `ignition-hostd`.
- Project, secret, and event public APIs (the API runs on seed `projects` rows; these surfaces are not exposed). A v0 image admission slice (`POST/GET .../images`, resolve-and-pin only — see [Image Data Layer](ignition-design-image-datalayer.md#security-status)) is exposed; the full image import/catalog contract is not.
- Writable Volume or Session snapshot resources.
- Multi-region active-active databases. Deployment is one region (`us-central1`) behind an optional global hostname.

## 3. Why two binaries

| Constraint | `ignition-api` | `ignition-controller` |
|---|---|---|
| Kubernetes API | **Forbidden** | Sole owner of Pod RBAC in `ignition-sandboxes` |
| Public network | Yes (Ingress / global HTTPS LB) | No |
| Cloud SQL role | Admit + read product rows | Reconcile + update sandbox/process/operation state |
| Blast radius | Stolen API identity cannot create Pods | Stolen controller identity cannot mint customer JWTs or exec stream tokens |
| Scale | Stateless replicas behind LB | Small replica set; leader-optional (see §6.3) |

A combined binary is rejected: the public listener would inherit Pod create/delete, and a controller compromise would inherit token minting.

Both binaries live in this repo (`cmd/ignition-api`, `cmd/ignition-controller`) and deploy as separate Deployments, Kubernetes ServiceAccounts, Workload Identity bindings, and database users.

## 4. Package layout

```text
cmd/ignition-api/              main
cmd/ignition-controller/       main
internal/api/                  HTTP server, routes, JSON codec, SSE
internal/auth/                 JWT validation, RBAC
internal/store/                Cloud SQL (pgx), transactions
internal/controller/           reconcile loop, Pod spec, balloons
internal/k8s/                  typed client, no use from api
internal/config/               env: listen, OIDC, Cloud SQL, region
api/proto/ignition/v1/         source of truth for resource shapes
api/gen/ignition/v1/           generated Go (do not edit)
```

`internal/api` must not import `internal/k8s` or `internal/controller`. Enforce with a package comment and a unit test that walks import graphs if needed.

## 5. `ignition-api`

### 5.1 Transport

- Listen on HTTP internally; TLS terminates at the Application Load Balancer.
- Public JSON uses proto `json_name` (lowerCamelCase). Public enums drop the prefix: `SANDBOX_STATE_READY` → `"READY"`.
- `Authorization: Bearer` on every route.
- `Idempotency-Key` required on create, terminate, operation cancel, and process create/attach/signal/cancel.
- Watch is **SSE** (`text/event-stream`). It emits a content-addressed snapshot whenever product state changes, honors `Last-Event-ID`, sends heartbeats, and closes on a terminal state or after ~60s.

gRPC may exist internally later. It is not the public v1 edge.

### 5.2 Authentication and authorization

`internal/auth` verifies, with JWKS (never the attach-stream HMAC) and ≤ 60s skew on `exp`/`iat` (`nbf` when present):

- **Cloud IAP assertions** (`X-Goog-IAP-JWT-Assertion`, ES256, issuer `https://cloud.google.com/iap`, `aud` = `IGNITION_IAP_AUDIENCE`) — preferred when the header is present;
- **Google ID tokens** (`Authorization: Bearer`, issuer `https://accounts.google.com`, `typ=JWT`, RS256, `aud` ∈ `IGNITION_OIDC_AUDIENCE` + `IGNITION_OIDC_AUDIENCES`, `email_verified`, and `hd` ∈ `IGNITION_OIDC_HOSTED_DOMAINS` for non-service-account subjects);
- **first-party RFC 9068 `at+jwt`** when `IGNITION_OIDC_ALLOWED_TYPES` includes `at+jwt`.

`IGNITION_OIDC_SUBJECT_CLAIM=email` makes the verified email the RBAC subject; a `*.gserviceaccount.com` email is a service account (hosted-domain check skipped, no role cap). Then load `role_bindings` for `(project_id, subject)` — exact subject first, then a `domain:<hd>` binding for a Workspace user. SQL is project-scoped **before** loading the object row. Cross-project or unknown IDs return indistinguishable `404 NOT_FOUND`. In-project missing permission returns `403 PERMISSION_DENIED` for **create/exec**; terminate and operation-cancel deny returns `404`.

Staging/prod (`IGNITION_ENV`) refuse to start with `IGNITION_DEV_BEARER`, a missing issuer, or the default stream secret. The `dev` overlay has no Ingress and uses `IGNITION_DEV_BEARER` (subject `dev`).

Because the Project API is not exposed yet, operators seed one `projects` row and bind the first `owner` via `IGNITION_BOOTSTRAP_PROJECT` + `IGNITION_BOOTSTRAP_ADMIN` or `db/rolebindings.sql`; `roleBindings` CRUD (`GET/PUT/DELETE /v1/projects/{project}/roleBindings/{subject}`, owner/admin only, last-owner guard) manages the rest.

Required permissions:

| RPC | Permission |
|---|---|
| CreateSandbox | `sandbox.create` |
| Get/List/WatchSandbox | `sandbox.get` |
| TerminateSandbox | `sandbox.terminate` |
| CreateProcess, Attach, Signal, Cancel | `sandbox.exec` |
| Get/ListProcess | `process.get` |
| Get/List/WatchOperation | `operation.get` |
| CancelOperation | `operation.cancel` |
| GetRuntimeDefault | `runtime.get` |
| CreateImage (v0 resolve-and-pin) | `image.create` |
| GetImage | `image.get` |
| List/GetRoleBinding | `rolebinding.get` (owner/admin) |
| Put/DeleteRoleBinding | `rolebinding.admin` (owner/admin) |

### 5.3 Create sandbox (admission)

One serializable Cloud SQL transaction. The API never creates a Pod.

1. Canonicalize the JSON body (sorted keys, normalized numbers). Hash with method + route + principal + project.
2. Insert `idempotency_keys`, or:
   - same hash → wait for commit and replay the stored response (`202`);
   - in progress → `409 IDEMPOTENCY_IN_PROGRESS` + `Retry-After`;
   - different hash → `409 IDEMPOTENCY_KEY_REUSED`.
3. Validate `imageId` charset and region. `resources`/`placement`/`timeouts`/`network` are optional: merge the request over the system [default runtime](ignition-design-default-runtime.md), then validate the resolved `RuntimeSpec` — accelerator allowlist (`IGNITION_ALLOWED_ACCELERATORS`, default `NONE,NVIDIA_L4`), per-type `accelerator.count`, CPU/memory/timeout caps, `computeEnvironment` and `internetAccess` enums. Validate `command`/`label` caps and `secretRefs` (charset, env names) separately.
4. Insert `sandboxes` (`state = CREATING`, `generation = 1`).
5. Insert `operations` (`kind = CREATE_SANDBOX`, `state = PENDING`).
6. Increment `project_quota.active`.
7. Commit. Return `202` `{ sandbox, operation }`.

On any validation failure before commit, return a stable `Status` (`INVALID_ARGUMENT`, `IMAGE_NOT_READY`, `QUOTA_EXCEEDED`, `WORKLOAD_NOT_SUPPORTED`, …) with `requestId`, `retryable`, optional `retryAfterSeconds`.

Startup deadline `timeouts.startupSeconds` is stored on the sandbox; the **controller** fails the sandbox with `CAPACITY_UNAVAILABLE` if `READY` is not reached in time. The API does not wait.

**Create image (v0 admission, `internal/imagecatalog`):** `POST .../images` runs synchronously, not in a Cloud SQL transaction against the request's own commit path — it resolves `sourceRef` against the source registry (no Ignition-owned copy), then inserts one `images` row keyed `(project_id, image_id)`. A primary-key collision fails closed to `409 IMAGE_ALREADY_EXISTS` rather than re-resolving or overwriting the pinned digest; there is no `Idempotency-Key` on this route. The resolver does not currently restrict which registry host `sourceRef` may name — see [Image Data Layer — Security status](ignition-design-image-datalayer.md#security-status) before relying on this endpoint being safe against an adversarial caller holding ordinary `image.create`.

### 5.4 Terminate

Idempotent mutation: set desired state `TERMINATING`, insert `TERMINATE_SANDBOX` operation if none is open, commit, return `202`. Already `FINISHED`/`FAILED` replays success. The controller deletes the Pod. Permission deny is `404`.

Cancel of an in-flight `CREATE_SANDBOX` (`POST …/operations/{id}:cancel`) sets the sandbox `FAILED` / `CANCELLED`, releases quota, and prevents Pod create. Permission deny is `404`.

### 5.5 Process (control plane only)

Require sandbox `READY` (`FAILED_PRECONDITION` otherwise).

- **Create:** insert `processes` (`CREATING`), persist argv/cwd/env/pty. Return the `Process` resource immediately. Do not stream stdout. The controller (or init supervisor via the gateway path) advances `STARTING` → `RUNNING`.
- **Attach:** mint a short-lived exec stream token (audience ≠ access JWT) bound to `project_id`, `sandbox_id`, `generation`, `process_id`, `stream_epoch`, action `attach`. Honor `Idempotency-Key` (replay the same epoch). Return `{ streamToken, gatewayUrl, expireTime, streamEpoch }`. Bytes never enter this process.
- **Signal / cancel:** idempotent row updates; cancel is graceful then kill per `terminationGraceSeconds`. Client disconnect does not change process state.

`gatewayUrl` is the **regional** gateway hostname (for example `https://gateway.us-central1.ignition.dev`). A token is invalid on any other region’s gateway.

### 5.6 Watch and list

- List: cursor pagination, order `(create_time, id)`, project filter only, SQL `LIMIT`.
- Watch: emit a full resource snapshot, then heartbeats, then close (~60s). Re-GET for later states until push-on-change exists.

### 5.7 Failure isolation

API crash after commit is correct: the Operation is durable and the controller proceeds. Clients reconnect with watch or `GET`. The API is horizontally scalable; no in-memory admission lock across replicas (idempotency table is the lock).

## 6. `ignition-controller`

### 6.1 Authority

Cloud SQL is authoritative for desired sandbox state. The Kubernetes watch is a hint. The controller is level-triggered: on every tick or event, compare SQL desired vs cluster observed and converge.

Pod name is deterministic: `sbx-{sandbox_id}` in namespace `ignition-sandboxes`. Create is `Create` with that name; `AlreadyExists` is success for this generation. Never generate a second name for the same `sandbox_id`.

### 6.2 Reconcile loop

```text
for each sandbox in SQL:
  CREATING + no Pod           → Create Pod (normative spec) if imageId is valid
  invalid imageId             → FAILED IMAGE_UNAVAILABLE (no Pod)
  Pod Scheduled               → SQL SCHEDULED
  container running           → SQL STARTED
  init healthy + GPU UUID annotation → SQL READY (kube Ready alone is STARTED)
  desired TERMINATING         → Delete Pod; on gone → FINISHED
  CREATE cancelled            → SQL already FAILED; do not Create
  startup deadline exceeded   → FAILED CAPACITY_UNAVAILABLE or STARTUP_TIMEOUT
  Pod gone unexpectedly       → FAILED WORKER_LOST; release quota; restore balloon
  GPU cleanup ambiguous       → GET node; cordon only if labeled gpu-sandbox-l4
```

The Pod spec is entirely server-owned. No client field maps to hooks, devices, hostPath, capabilities, or scheduling. Normative YAML: [GKE Sandbox — Sandbox Pod profile](ignition-design-gke-sandbox.md#sandbox-pod-profile-normative).

State mapping:

| SQL public state | Observation |
|---|---|
| `CREATING` | admitted; Pod missing or not scheduled |
| `SCHEDULED` | `PodScheduled=True` |
| `STARTED` | container running; readiness incomplete |
| `READY` | CPU: kube `PodReady`. GPU: kube `PodReady` **and** `ignition-gpu-agent` has stamped `ignition.io/init-healthy=true` + a canonical `ignition.io/gpu-uuid` (`GPU-…`); `PodReady` alone is `STARTED` |
| `FAILED` | terminal Pod failure, deadline, image error, node loss |
| `TERMINATING` / `FINISHED` | terminate requested / Pod deleted and cleanup verified |

### 6.3 Concurrency

Run **two replicas** with a SQL lease (`controller_leases`, `FOR UPDATE SKIP LOCKED` or a single-row epoch). Only the lease holder mutates Pods. The standby refreshes and takes over on expiry (default 10s). Both may read. This avoids duplicate create races even though Pod names are deterministic.

Do not use Kubernetes leader election as the only lock: Cloud SQL must still be the source of desired state if the API server is partitioned.

### 6.4 Warm capacity

Sandbox Pods scale to zero; GPU **nodes** must not. The controller maintains balloon Pods (`priorityClassName: ignition-balloon`, negative priority, `nvidia.com/gpu: "1"`) so Cluster Autoscaler keeps `target_warm` Ready nodes.

```text
target_warm = max(min_warm, ceil(p95_creates_per_minute × node_provision_minutes))
```

Start with `min_warm = 1` or `2`, `max_nodes` capped by quota and budget. Cooldown before scale-in: 15 minutes. Annotate nodes that host a customer sandbox with `cluster-autoscaler.kubernetes.io/scale-down-disabled=true`.

A real sandbox Pod (`priorityClassName: ignition-sandbox`) preempts a balloon. Autoscaler replenishes the balloon in the background; that replenishment is **outside** the 9s SLO.

### 6.5 Secrets

Resolve Secret Manager refs at Pod create using the controller’s Google identity. Inject as container env in the Pod spec. Do not create cluster Secret objects that other namespaces can read. Sandbox Pods have `automountServiceAccountToken: false` and no Workload Identity.

### 6.6 Process observation

The controller does not proxy stdio. It publishes desired process argv/signal/cancel as the `ignition.io/process-desired` annotation and advances `processes.state` from `ignition.io/process-observed` (written by the init supervisor). Failed create of the in-sandbox process sets `FAILED` with a typed reason. Signal and cancel remain SQL desired-state until init reports `EXITED`/`FAILED`.

## 7. Data model

```text
projects
role_bindings
images                 -- seed rows; Image APIs not exposed
sandboxes
processes
operations
idempotency_keys       -- principal, project, method, route, body_hash, response
project_quota          -- active sandbox count (not an append-only ledger)
controller_leases
```

This is a complete baseline schema (`internal/store/schema.sql`, embedded), not a migration chain. Every customer-owned row has non-null `project_id`. Indexes: `(project_id, id)`, `(project_id, state)`, `(sandbox_id)` on processes.

Database: Cloud SQL for PostgreSQL (regional HA on dev, zonal on staging), private IP, Auth Proxy sidecar, Workload Identity. A password DSN in `DATABASE_URL` is used; IAM DB users remain the staging/prod hardening path.

`ignition-api` owns DDL (`store.Open`). `ignition-controller` is DML only (`store.OpenWithoutSchema`).

## 8. Identity and IAM (runtime)

| Workload | KSA | GCP SA | K8s RBAC | Cloud SQL |
|---|---|---|---|---|
| `ignition-api` | `ignition-system/ignition-api` | `ignition-api@PROJECT` | none | DML: product + idempotency + quota |
| `ignition-controller` | `ignition-system/ignition-controller` | `ignition-controller@PROJECT` | Pods in `ignition-sandboxes`; get/list/patch Nodes, cordon only if `ignition.io/node-pool=gpu-sandbox-l4` | DML: sandbox/process/operation/lease (no DDL) |
| `ignition-gateway` | `ignition-system/ignition-gateway` | `ignition-gateway@PROJECT` | none (or get Pod IP in sandboxes if required) | read routes / process attach metadata |

No cluster-admin. No access to `kube-system`. GPU nodes are private. The API records the requested internet-access profile, while GCP projects, VPCs, subnets, firewall policy, and NAT enforce it. The controller does not translate client input into Kubernetes NetworkPolicy rules.

## 9. Deployment topology

**Compute is regional.** One GKE cluster and one Cloud SQL instance in `us-central1` (three zones). See [Implementation guide](../guides/ignition-implementation.md#deploy-regional-dev).

**Hostname may be global.** `api.ignition.dev` is an anycast HTTPS frontend whose backend is this region’s `ignition-api`. Exec uses a regional `gatewayUrl`. Do not run a globally writable Postgres. A second region later is a second full stack plus routing on `placement.region`.

Replicas (initial): API 3, controller 2, gateway 3; PDBs; topology spread across zones.

## 10. Observability

Metrics (region, SKU, project labels where safe):

- API: request count, latency, `202`/`4xx`/`5xx`, idempotency replay vs conflict.
- Admission: transaction time (budget 0.5s p95).
- Controller: pickup lag, Pod create latency, per-stage startup vs 9s budget, reconcile errors, lease holder.
- Warm: buffer size vs target, balloon preemptions, node provision time.
- Capacity: queue age, `CAPACITY_UNAVAILABLE` rate.

Traces: `request_id` from API through SQL operation id to Pod name. Logs exclude command argv payloads, env values, stdin/stdout, and secret material.

## 11. Delivery status

| Step | State |
|---|---|
| `internal/store` + schema + idempotency tests | done |
| Auth middleware + create/get returning `202` | done (Google OIDC / IAP, not the earlier Auth0 plan) |
| Controller create/delete on the CPU gVisor pool | done — CPU lifecycle verified end to end |
| L4 sandbox pool + one-sandbox-per-node + `ignition-gpu-agent` attestation | code done; a real L4 sandbox reaching `READY` is not yet exercised (dev L4 quota) |
| Watch/SSE | done |
| `ignitionctl --wait` | not started (`ignitionctl` is a stub) |
| Process rows + attach-token minting | done (rows + token); gateway byte path not built |
| Balloons + p95 API-to-`READY` measurement | balloons implemented; dev runs with `IGNITION_MIN_WARM=0`, not measured |

## 12. Acceptance

- Concurrent identical `Idempotency-Key` yields one sandbox, one Pod, one operation.
- Mutated body with the same key returns `IDEMPOTENCY_KEY_REUSED`.
- `ignition-api` binary has no kubernetes client in its import graph and no RBAC in its KSA.
- Controller restart at every public state still converges; no duplicate Pods.
- Two sandbox Pods cannot schedule on one L4 node (GPU request and hostname anti-affinity).
- Sandbox Pod has no service-account token and cannot reach metadata or the Kubernetes API.
- Exec attach is p95 ≤ 1s from authenticated attach on a `READY` sandbox (gateway SLO; API only mints the token).
- Warm-path p95 API-to-`READY` ≤ 9s with balloons at target; cold node is out of SLO.

## 13. Decision summary

| Decision | Choice |
|---|---|
| Language | Go 1.26 |
| Public API | HTTP/JSON + SSE; proto schema |
| Desired state | Cloud SQL; API writes, controller reads |
| Kubernetes | Controller only; deterministic Pod names |
| Isolation | GKE Sandbox, one L4 per node, one customer sandbox per node |
| Global | Hostname/LB only; control plane and GPUs stay regional |
| Next region | Clone the stack; route on `placement.region` |
