# Ignition API contract

**Status: SHIPPED** for identity, project RBAC, the sandbox / process / operation
API, the exec byte stream, `ignitionctl`, and the first Python/TypeScript SDK
slice. **PROPOSED:** Project / Image (full) / Secret / Event resources, richer SDK
streaming. Volumes and SESSION snapshots are out of scope.

The request/response schema, state machines, idempotency, and error model here
are the canonical public contract and are runtime-agnostic. Provisioning behind
the contract is in [shipped architecture](ignition-shipped-architecture.md).

**Machine-readable schema:** [`api/proto/ignition/v1/`](../../api/proto/ignition/v1/)
(`SandboxService`, `OperationService`) · [`api/openapi/v1.yaml`](../../api/openapi/v1.yaml)

## 1. Identity

External identity is Google. Ignition implements no passwords, MFA, or primary
identity storage. `ignition-api` accepts, in order of preference:

| Path | Caller | Credential | Verification |
|---|---|---|---|
| Through the Ingress | Human Workspace users, external automation | Cloud IAP browser flow → `X-Goog-IAP-JWT-Assertion` | issuer `https://cloud.google.com/iap`, ES256, `aud` = `IGNITION_IAP_AUDIENCE` |
| In-cluster / impersonation | Probers, CI, service-to-service | `Authorization: Bearer <Google ID token>` | issuer `https://accounts.google.com`, `typ=JWT`, RS256, `aud` ∈ `IGNITION_OIDC_AUDIENCE(S)`, `email_verified`, and (users only) `hd` ∈ `IGNITION_OIDC_HOSTED_DOMAINS` |
| Optional | first-party automation | RFC 9068 `at+jwt` | when `IGNITION_OIDC_ALLOWED_TYPES` includes `at+jwt` |

The middleware verifies the IAP header when present, otherwise the bearer.
Validators use JWKS (pinned where node egress can't reach OIDC discovery),
refresh once on an unknown key ID, fail closed otherwise, and allow ≤ 60s skew.
Both paths resolve to one `Principal{Subject, Email, Kind, Domain}` — with
`IGNITION_OIDC_SUBJECT_CLAIM=email` the verified email is the RBAC subject — and
one SQL project RBAC check.

A `*.gserviceaccount.com` email is a **service account**: exempt from the
hosted-domain check and **not** privilege-capped (it may hold any role, including
`owner`). A service account is not a role. There is no first-party OAuth
provider, PKCE/device flow, or API-key exchange. Wrong issuer/audience/type,
stale JWKS keys, excessive skew, unverified email, a disallowed hosted domain,
and cross-class tokens fail closed with `401`. Emergency revocation is
Google-side disablement.

**Credential classes are distinct and non-interchangeable:** (1) external Google
ID tokens / IAP assertions for control-plane calls; (2) short-lived exec stream
tokens bound to project + sandbox + process + stream epoch + action
(`IGNITION_STREAM_TOKEN_SECRET`); (3) internal route tokens + SPIFFE mTLS —
[deferred runtime](ignition-deferred-runtime.md) only. No validator accepts a
token from another class.

## 2. Resource hierarchy

```text
organization
└── project              -- resource, quota, authorization, idempotency, fairness boundary
    ├── image
    ├── secret
    ├── sandbox
    │   └── process
    ├── operation
    └── event
```

Organization is the billing/policy boundary. Every customer-owned row has a
non-null `project_id`. Users and service accounts are **principals**. SQL is
project-scoped before object rows are loaded; attribute checks bind children to
their parent sandbox; denial is the default.

For an ID that does not exist **or belongs to another project**, return `404
NOT_FOUND` with indistinguishable shape and timing. Return `403
PERMISSION_DENIED` only when the object is in the requested project and the
principal lacks **create** or **exec**. **Terminate and operation-cancel** deny
on a known in-project object also return `404`.

### Project RBAC

A project always retains ≥ 1 `owner` (last-owner guard). `admin` manages access
but cannot transfer/delete the project or alter owners. Role lookup resolves the
exact subject, then a `domain:<hd>` binding for a Workspace user. **Enforced
today:** `sandbox.*`, `process.get`, `operation.*`, `image.create`, `image.get`,
`rolebinding.*`, `runtime.get`. Rows for unexposed resources describe the
intended model.

| Permission | owner | admin | developer | operator | viewer |
|---|---:|---:|---:|---:|---:|
| `project.get`, `event.list` | ✓ | ✓ | ✓ | ✓ | ✓ |
| `project.update` | ✓ | ✓ | | | |
| `project.delete`, `project.owner.manage` | ✓ | | | | |
| `project.iam.manage` (non-owner bindings) | ✓ | ✓ | | | |
| `image.create` (v0 resolve + pin), `image.get` | ✓ | ✓ | ✓ | ✓ | get only |
| `image.import`, `image.delete` *(not built)* | ✓ | ✓ | ✓ | | |
| `secret.create/version/rotate/delete` *(not built)* | ✓ | ✓ | ✓ | | |
| `secret.getMetadata`, `secret.list`, `secret.use` *(not built)* | ✓ | ✓ | ✓ | ✓ | metadata/list |
| `sandbox.create`, `sandbox.exec` | ✓ | ✓ | ✓ | ✓ | |
| `sandbox.terminate`, `operation.cancel` | ✓ | ✓ | own | ✓ | |
| `sandbox.get/list`, `process.get`, `operation.get` | ✓ | ✓ | ✓ | ✓ | ✓ |

`own` = the principal initiated the sandbox/operation. Custom roles and
resource-level sharing are out of scope. SQL `role_bindings` + explicit ownership
checks are authoritative.

## 3. Endpoints

**Served today** (`SandboxService` / `OperationService`, plus `roleBindings` and
the default-runtime read):

```text
POST   /v1/projects/{project}/sandboxes
GET    /v1/projects/{project}/sandboxes
GET    /v1/projects/{project}/sandboxes/{sandbox}
POST   /v1/projects/{project}/sandboxes/{sandbox}:terminate
GET    /v1/projects/{project}/sandboxes/{sandbox}:watch

POST   /v1/projects/{project}/sandboxes/{sandbox}/processes
GET    /v1/projects/{project}/sandboxes/{sandbox}/processes
GET    /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:attach
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:signal
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:cancel

GET    /v1/projects/{project}/operations
GET    /v1/projects/{project}/operations/{operation}
GET    /v1/projects/{project}/operations/{operation}:watch
POST   /v1/projects/{project}/operations/{operation}:cancel

GET    /v1/projects/{project}/runtimes/default

POST   /v1/projects/{project}/images                 -- v0 slice; see Image delivery
GET    /v1/projects/{project}/images/{image}

GET    /v1/projects/{project}/roleBindings
PUT    /v1/projects/{project}/roleBindings/{subject} -- {subject} = email or domain:<fqdn>; owner/admin, last-owner guard, audit line
DELETE /v1/projects/{project}/roleBindings/{subject}
```

**Specified, not exposed:** full `projects` CRUD, `images:import` / list /
`/status` / delete, the `secrets` lifecycle, and the `events` feed
(`GET /v1/projects/{project}/events[:watch]`, `…/events/{event}`). Shapes follow
the same conventions: immutable opaque IDs, cursor pagination ordered
`(create_time, id)`, write-only secret payloads, `202` + Operation for long work.

There are no Volume, sandbox volume-mount, or SESSION snapshot endpoints.

## 4. CreateSandbox

```http
POST /v1/projects/{project_id}/sandboxes
Authorization: Bearer ACCESS_TOKEN
Idempotency-Key: UUID
Content-Type: application/json
```

```json
{
  "name": "model-runner",
  "imageId": "img_01J...",
  "command": ["python", "-m", "server"],
  "workingDirectory": "/workspace",
  "nativeEntrypoint": false,
  "secretRefs": [{ "secretId": "sec_01J...", "version": "latest", "environmentName": "MODEL_TOKEN" }],
  "resources": { "cpuMilli": 4000, "memoryMiB": 16384, "accelerator": { "type": "NVIDIA_L4", "count": 1 } },
  "placement": { "region": "us-central1", "computeEnvironment": "STANDARD" },
  "timeouts": { "startupSeconds": 120, "maximumRuntimeSeconds": 3600, "idleSeconds": 600, "terminationGraceSeconds": 20 },
  "network": { "internetAccess": "DISABLED" },
  "labels": { "team": "inference" }
}
```

| Field | Rule |
|---|---|
| `imageId` | **required**; `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`; resolved under the Artifact Registry sandbox prefix (digest pinning is future work) |
| `command` | optional argv, never shell-evaluated; when omitted, `sandbox-init` waits for `exec`; ignored (not rejected) when `nativeEntrypoint` is `true` |
| `nativeEntrypoint` | default `false`; `true` runs the image's own `Entrypoint`/`Cmd` as PID 1 — weaker readiness, no exec/idle-tracking, same security context (see [shipped architecture §5](ignition-shipped-architecture.md#isolation-invariants)) |
| `secretRefs` | stored on create; resolved from Secret Manager and injected as env at Pod create; values never enter SQL |
| `resources` / `placement` / `timeouts` / `network` | **optional** — unset fields come from the [default runtime](ignition-shipped-architecture.md#6-default-runtime) (CPU-only); the resolved `RuntimeSpec` is snapshotted onto the sandbox |
| `accelerator.type` | `NVIDIA_L4` or `NONE`; `IGNITION_ALLOWED_ACCELERATORS` default `NONE,NVIDIA_L4`; no profile → `WORKLOAD_NOT_SUPPORTED`, no Pod |
| `accelerator.count` | `1` for `NVIDIA_L4`, `0`/absent for `NONE` |
| `cpuMilli` / `memoryMiB` | positive, ≤ 8000 / ≤ 32768 |
| timeouts | `startupSeconds` ≤ 600, `maximumRuntimeSeconds` ≤ 86400, `idleSeconds` ≤ 3600, `terminationGraceSeconds` ≤ 120 |
| `computeEnvironment` | `STANDARD` (default, hides the managed runtime) or `BARE_METAL` (never falls back to `STANDARD`; today the operation moves to `FAILED` `COMPUTE_ENVIRONMENT_UNAVAILABLE`) |
| `network.internetAccess` | `DISABLED` (default) or `ENABLED`; outbound public only, never inbound; GCP networking enforces it |
| `labels` | ≤ 32; reserved `ignition.*` keys rejected |

User-supplied OCI hooks, host mounts, devices, CDI records, capabilities,
namespaces, and readiness probes are not accepted.

**Response** — `202 Accepted`, `Location`, `Retry-After: 1`:

```json
{
  "sandbox":   { "id": "sbx_01J...", "projectId": "prj_01J...", "state": "CREATING", "stateReason": "ADMITTED", "imageId": "img_01J...", "operationId": "op_01J...", "createdAt": "..." },
  "operation": { "id": "op_01J...", "kind": "CREATE_SANDBOX", "state": "PENDING", "resourceId": "sbx_01J...", "createdAt": "..." }
}
```

The response confirms durable admission, not readiness. `Idempotency-Key` reuse
with the same canonical request replays the original sandbox + operation;
different content → `409 IDEMPOTENCY_KEY_REUSED`.

## 5. State machines

```text
Sandbox:  CREATING → SCHEDULED → STARTED → READY → TERMINATING → FINISHED
          any nonterminal → FAILED

Process:  CREATING → STARTING → RUNNING → EXITED
                             ↘ CANCELLING → EXITED
          any pre-terminal → FAILED
```

- `CREATING` admission durable, Pod not scheduled · `SCHEDULED`
  `PodScheduled=True` · `STARTED` container running, readiness incomplete ·
  `READY` kubelet `PodReady`, and for `NVIDIA_L4` an `ignition-gpu-agent`-stamped
  canonical GPU UUID + `init-healthy` annotation. There is no user-configured
  readiness probe; application health is the application's responsibility. A
  `READY` sandbox serves exec.
- `EXITED` carries an exit code or terminating signal and immutable exit time.
  `FAILED` carries a typed reason. Client disconnect never changes process state.
  Cancellation sends a graceful signal, waits the grace period, then kills;
  repeated cancellation is idempotent.

`Idempotency-Key` is required for every create and retriable mutation (including
action endpoints); `GET`, complete-`PUT`, and `DELETE` accept but do not require
it. A key is scoped to principal + organization + project + method + canonical
route, stores a canonical request hash (sorted keys, normalized numbers) and the
replayable committed result for ≥ 24h. Same-hash concurrent duplicate waits for
the result or gets `409 IDEMPOTENCY_IN_PROGRESS` + `Retry-After`; different-hash
reuse → non-retryable `409 IDEMPOTENCY_KEY_REUSED`. Retry after expiry may create
a new side effect; SDKs warn.

Watch endpoints use authenticated SSE — content-addressed snapshot on change,
`Last-Event-ID`, heartbeats, close on terminal state or ~60s.

## 6. Errors and quotas

```text
400 INVALID_ARGUMENT     401 UNAUTHENTICATED     403 PERMISSION_DENIED
404 NOT_FOUND            409 IDEMPOTENCY_KEY_REUSED / IMAGE_NOT_READY
422 WORKLOAD_NOT_SUPPORTED   429 QUOTA_EXCEEDED / RATE_LIMITED
503 CAPACITY_UNAVAILABLE / UNAVAILABLE
```

```json
{ "error": { "code": "CAPACITY_UNAVAILABLE", "message": "...", "requestId": "req_01J...",
             "retryable": true, "retryAfterSeconds": 10, "details": { "region": "us-central1", "accelerator": "NVIDIA_L4" } } }
```

Every error carries a stable code, request ID, retryability, optional retry
delay, and structured details. Quotas cover projects, active GPUs, creates,
execs, streams, image/artifact bytes, process count, retained output bytes, and
network connections.

## 7. Exec / attach

Client-facing protocol (mechanism in [shipped architecture §8](ignition-shipped-architecture.md#8-exec-data-plane)):

1. `POST …/processes` creates the process; returns immediately.
2. `POST …/processes/{process}:attach` mints `{ streamToken, gatewayUrl,
   expireTime, streamEpoch }`. `gatewayUrl` is **regional**.
3. Open a WebSocket to `gatewayUrl` + `/v1/attach?token=<streamToken>`. Frames
   carry a channel (`stdin`/`stdout`/`stderr`/`control`) and kind; `[]byte`
   payloads are base64. A terminal `control`/`exit` frame carries the exit code
   or signal.
4. Reconnect is available for **10 minutes** after process exit against the
   in-memory replay buffer. PTY is accepted but not yet allocated. Durable
   offset/ACK reconnect is [deferred](ignition-deferred-runtime.md).

For a `READY` sandbox, successful attach has **p95 latency ≤ 1s** from the
gateway receiving an authenticated request to the client receiving the stream
acknowledgement.

## 8. `ignitionctl`

`internal/cli` — a dependency-light client over the same v1 REST API; it never
talks to Kubernetes.

```text
ignitionctl login --server https://... --token "$TOKEN" --project prj_01J...
ignitionctl whoami | logout | projects
ignitionctl config                          # print context; set-server <url> | set-project <id>

ignitionctl sandbox create --image img_01J... --accelerator NVIDIA_L4 --cpu 4000 --memory 16384 --wait
ignitionctl sandbox list | get <sbx> | terminate <sbx> --wait
ignitionctl exec <sbx> -- nvidia-smi                 # live stdio via ignition-gateway
ignitionctl exec --no-stream <sbx> -- true           # poll process state instead
ignitionctl process {list|get|signal|cancel} ...
ignitionctl operation {list|get|watch|cancel} ...
```

- Context in `~/.config/ignition/config.json` (`$IGNITION_CONFIG`); `IGNITION_SERVER`
  / `IGNITION_TOKEN` / `IGNITION_PROJECT` and per-command flags override it.
- Global flags (`--server`, `--project`, `-o json`, `--timeout`) accepted before
  or after the subcommand. `exec` keeps kubectl-style `flags then <sandbox> --
  <cmd>` so guest flags pass through.
- Every mutation sends an `Idempotency-Key`. `--wait` polls the Operation with
  backoff.
- Exit codes: `0` success, `2` usage, `3` not-found, `4` denied, `5`
  unauthenticated; `exec` propagates the guest exit code.
- `exec` streams through `ignition-gateway` when the attach response carries a
  `gatewayUrl`, and polls otherwise (or with `--no-stream`).

## 9. SDKs

- **Python `ignition-sandbox`** — sync + native async clients, bounded batch.
- **TypeScript `@ignition/sandbox`** — native promises + async iterators, bounded
  batch.

Both are implemented for the shipped control-plane lifecycle (`Sandbox` +
`Process` + `Operation`).

```python
with Client() as client:
    sandbox = client.sandboxes.create(
        project="prj_...", image="img_...", command=["python", "-m", "server"],
        resources=Resources(accelerator=Accelerator(type="NVIDIA_L4", count=1)),  # omit for CPU
    )
    sandbox.wait_ready(timeout=120)
    process = sandbox.exec(["nvidia-smi"])
    for chunk in process.stdout.iter_bytes():
        consume(chunk)
    code = process.wait()
    sandbox.terminate(wait=True)
```

**PROPOSED** target contract: `Project` / `Image` / `Secret` / `Event` /
`StreamReader` / `StreamWriter` handles; text wrappers with an incremental
decoder across frame boundaries; `iter_lines()` that preserves partial final
lines; bounded writer flow control; cancellation that does not implicitly cancel
a remote Process; a fresh Google ID token before control calls and a new stream
credential for reconnect (never one class refreshed as another); context managers
where exiting an attachment closes only the local stream and exiting a sandbox
terminates only if it created the sandbox with `terminate_on_exit=True`. No
`Volume` or session-snapshot handle exists.

## 10. Security invariants

- One hostile project sandbox per GPU VM; exactly one active lease per GPU (GKE:
  one Pod per node + whole-GPU request).
- Client input cannot add hooks, devices, host mounts, capabilities, or
  namespaces.
- The sandbox receives no Google identity, no broad project artifact / Secret
  Manager credential.
- `READY` is impossible before GPU verification.
- Sandbox traffic never uses the control-plane API path; GCE metadata,
  management sockets, and other sandbox addresses are unreachable.

## 11. Acceptance

- REST, Python, TypeScript, and CLI pass one black-box conformance suite over
  every public v1 resource above.
- Schema/route tests prove Volume and SESSION snapshot resources are absent from
  v1.
- Role-matrix tests cover every permission for user and service-account
  principals; a service account cannot be assigned as a role.
- Every endpoint returns indistinguishable `404` for nonexistent and
  cross-project IDs, `403` only for a known in-project denied create/exec.
- Authorization queries cannot load an object before applying its project scope.
- Correct-issuer/`aud`/type tokens authenticate; wrong issuer/audience/type,
  stale JWKS, excessive skew, unverified email, disallowed hosted domain, and
  cross-class tokens fail closed with `401`. A subject with no `role_bindings`
  row (and no `domain:` fallback) is denied.
- Concurrent same-hash idempotent requests → one side effect, one replayed
  response; different-hash reuse conflicts; records replayable for 24h.
- Every Process transition, cancel race, disconnect, exit code, signal, and
  startup failure is deterministic; client disconnect never changes process
  state.
- Exec attach meets p95 1s on a `READY` sandbox; reconnect stays available for 10
  minutes after process exit.
- Concurrent identical `Idempotency-Key` on create → one sandbox, one Pod, one
  operation; racing creates for one GPU → one active lease.
- From inside a sandbox: no second GPU, no metadata service, no host mounts, no
  worker credentials.
