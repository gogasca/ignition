# Ignition Client API and Identity Design

**Status:** Current for identity, project RBAC, the sandbox/process/operation API, and the first Python/TypeScript SDK slice. The full resource set (Project/Image/Secret/Event, Volumes, Session snapshots) is not built.

**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines the public API, external identity, project authorization, idempotency, errors, and compatibility. The API does **not** expose persistent writable Volume resources or SESSION memory-snapshot resources.

**Machine-readable schema:** [`api/proto/ignition/v1/`](../../api/proto/ignition/v1/) (`SandboxService`, `OperationService`). The exposed surface is sandbox lifecycle, the process control plane, operations, project `roleBindings`, and the default-runtime read. Project, image, secret, and event endpoints below are specified but not exposed. See the [Implementation guide](../guides/ignition-implementation.md).

## Components

- `ignition-api`: resource CRUD, authentication, authorization, operations.
- `ignitionctl`: human and automation CLI — specified; the binary exists but every subcommand returns `not implemented`. Use `curl` against the API.
- Python package `ignition-sandbox` — implemented for the shipped control-plane lifecycle, with sync/async clients and bounded batch execution.
- TypeScript package `@ignition/sandbox` — implemented for the shipped control-plane lifecycle and bounded batch execution.
- Google as the identity provider (Google Workspace accounts and GCP service accounts). Ignition does not implement passwords, MFA, or primary identity storage.

## External authentication contract

`ignition-api` accepts two Google-issued credential paths, plus an optional first-party token type. Both paths resolve to one `Principal{Subject, Email, Kind, Domain}` — with `IGNITION_OIDC_SUBJECT_CLAIM=email` the verified email is the RBAC subject — and the same project RBAC.

| Path | Caller | Credential | Verification |
| --- | --- | --- | --- |
| Through the Ingress | Human Workspace users, external automation | Cloud IAP browser flow → `X-Goog-IAP-JWT-Assertion` | issuer `https://cloud.google.com/iap`, ES256, `aud` = `IGNITION_IAP_AUDIENCE` (the backend-service resource path) |
| In-cluster / impersonation | Probers, CI, service-to-service | `Authorization: Bearer <Google ID token>` | issuer `https://accounts.google.com`, `typ=JWT`, RS256, `aud` ∈ `IGNITION_OIDC_AUDIENCE` + `IGNITION_OIDC_AUDIENCES`, `email_verified`, and (users only) `hd` ∈ `IGNITION_OIDC_HOSTED_DOMAINS` |

The middleware verifies the IAP assertion header when present, otherwise the bearer. Validators use JWKS (pinned to `https://www.googleapis.com/oauth2/v3/certs` where node egress cannot reach the OIDC discovery document), refresh once on an unknown key ID, fail closed if a usable key cannot be obtained, and allow at most 60 seconds of skew on `iat`/`nbf`/`exp`.

A `*.gserviceaccount.com` email is classified as a service account: exempt from the hosted-domain check and **not** privilege-capped — a service account may hold any role, including `owner`. First-party RFC 9068 `at+jwt` access tokens are also accepted when `IGNITION_OIDC_ALLOWED_TYPES` includes `at+jwt`; there is no first-party OAuth provider, PKCE/device flow, or API-key exchange.

ID tokens issued for other audiences, IAP assertions with the wrong `aud`, garbage, and cross-class tokens fail closed with `401`. Emergency revocation is Google-side disablement of the user or service account.

Credential classes are distinct and non-interchangeable:

1. external Google ID tokens / IAP assertions for public control-plane API calls;
2. short-lived exec stream tokens bound to project, sandbox, process, stream epoch, and action (`IGNITION_STREAM_TOKEN_SECRET`);
3. internal route tokens plus SPIFFE mTLS identities for gateway-to-ingress routing — part of the deferred custom runtime.

No validator accepts a token issued for another class or audience.

## Resource hierarchy and principals

The canonical hierarchy is:

```text
organization
└── project
    ├── image
    ├── secret
    ├── sandbox
    │   └── process
    ├── operation
    └── event
```

Users and service accounts are **principals**. A service account is not a role. Organization membership permits discovery of a project; project role bindings grant actions within that project. SQL queries are project-scoped before object rows are loaded, attribute checks bind children to their parent sandbox, and denial is the default.

### Role/permission matrix

A project must always retain at least one `owner` (the API's last-owner guard enforces this). `admin` can manage access but cannot transfer/delete the project or alter owners. Rows for resources that are not yet exposed (image, secret, event, `project.*`) describe the intended model; only the `sandbox.*`, `process.get`, `operation.*`, and `rolebinding.*` permissions are enforced today. Role lookup resolves the exact subject, then a `domain:<hd>` binding for a Workspace user.

| Permission | owner | admin | developer | operator | viewer |
|---|---:|---:|---:|---:|---:|
| `project.get`, `event.list` | yes | yes | yes | yes | yes |
| `project.update` | yes | yes | no | no | no |
| `project.delete`, `project.owner.manage` | yes | no | no | no | no |
| `project.iam.manage` (non-owner bindings) | yes | yes | no | no | no |
| `image.import`, `image.delete` | yes | yes | yes | no | no |
| `image.get`, `image.list` | yes | yes | yes | yes | yes |
| `secret.create`, `secret.version`, `secret.rotate`, `secret.delete` | yes | yes | yes | no | no |
| `secret.getMetadata`, `secret.list` | yes | yes | yes | yes | yes |
| `secret.use` | yes | yes | yes | yes | no |
| `sandbox.create`, `sandbox.exec` | yes | yes | yes | yes | no |
| `sandbox.terminate` | yes | yes | own | yes | no |
| `sandbox.get`, `sandbox.list`, `process.get`, `operation.get` | yes | yes | yes | yes | yes |
| `operation.cancel` | yes | yes | own | yes | no |

`own` means the principal initiated the sandbox or operation. Custom roles and resource-level sharing are out of scope. SQL-backed RBAC (`role_bindings`) and explicit ownership/context checks are authoritative.

For an object ID that does not exist **or belongs to another project**, return `404 NOT_FOUND` with indistinguishable response shape and timing bounds. Return `403 PERMISSION_DENIED` when the object is in the requested project but the principal lacks **create** or **exec**. **Terminate and operation-cancel** deny on a known in-project object also return `404`, so existence is not leaked by the permission check.

## Public resources and endpoints

All resource names are immutable opaque IDs. List APIs use cursor pagination and deterministic `(create_time, id)` ordering. Secret payloads are write-only and never returned.

The normative request, response, state progression, failure behavior, and acceptance tests for sandbox creation are defined in [Create Sandbox API Specification](ignition-sandbox-create-api.md). Provisioning behavior behind that contract (controller reconciliation, warm GPU node pools, the warm-capacity startup SLO) is defined in the [GKE Sandbox](ignition-design-gke-sandbox.md) design.

### Exposed endpoints

These endpoints are served today (`SandboxService` / `OperationService`, plus `roleBindings` and the default-runtime read):

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

GET    /v1/projects/{project}/roleBindings
PUT    /v1/projects/{project}/roleBindings/{subject}
DELETE /v1/projects/{project}/roleBindings/{subject}
```

`{subject}` is an email or `domain:<fqdn>`. `roleBindings` write access is owner/admin only, with a last-owner guard and an audit-log line per mutation.

### Full contract (specified, not exposed)

All resource names are immutable opaque IDs. List APIs use cursor pagination and deterministic `(create_time, id)` ordering. Secret payloads are write-only and never returned.

```text
# Project
POST   /v1/projects
GET    /v1/projects
GET    /v1/projects/{project}
PATCH  /v1/projects/{project}
DELETE /v1/projects/{project}
GET    /v1/projects/{project}/roleBindings
PUT    /v1/projects/{project}/roleBindings/{binding}
DELETE /v1/projects/{project}/roleBindings/{binding}

# Image import and status
POST   /v1/projects/{project}/images:import
GET    /v1/projects/{project}/images
GET    /v1/projects/{project}/images/{image}
GET    /v1/projects/{project}/images/{image}/status
DELETE /v1/projects/{project}/images/{image}

# Secret lifecycle
POST   /v1/projects/{project}/secrets
GET    /v1/projects/{project}/secrets
GET    /v1/projects/{project}/secrets/{secret}
POST   /v1/projects/{project}/secrets/{secret}/versions
POST   /v1/projects/{project}/secrets/{secret}:rotate
DELETE /v1/projects/{project}/secrets/{secret}

# Sandbox
POST   /v1/projects/{project}/sandboxes
GET    /v1/projects/{project}/sandboxes
GET    /v1/projects/{project}/sandboxes/{sandbox}
POST   /v1/projects/{project}/sandboxes/{sandbox}:terminate

# Process and exec attachment
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes
GET    /v1/projects/{project}/sandboxes/{sandbox}/processes
GET    /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:attach
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:signal
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:cancel

# Long-running operation
GET    /v1/projects/{project}/operations
GET    /v1/projects/{project}/operations/{operation}
GET    /v1/projects/{project}/operations/{operation}:watch
POST   /v1/projects/{project}/operations/{operation}:cancel

# Immutable audit/lifecycle event feed
GET    /v1/projects/{project}/events
GET    /v1/projects/{project}/events/{event}
GET    /v1/projects/{project}/events:watch
```

Image import, sandbox creation/termination, secret rotation/deletion, and other long work return `202` with an Operation. Watch endpoints use authenticated Server-Sent Events, emit content-addressed snapshots when product state changes, honor `Last-Event-ID`, send heartbeats, and close on terminal state or after ~60s. Cancellation of an in-flight `CREATE_SANDBOX` fails the sandbox (`CANCELLED`) and releases quota; otherwise the Operation records whether work was cancelled or had already reached a non-cancellable/terminal point.

There are no Volume, sandbox volume-mount, or Session snapshot endpoints.

## Idempotency

`Idempotency-Key` is required for every create and retriable mutation, including action endpoints; naturally idempotent `GET`, `PUT` of a complete role binding, and `DELETE` still accept but do not require it. A key is scoped to authenticated principal, organization (and project when one exists), HTTP method, and canonical route.

The server computes a canonical request hash from method, canonical route parameters, normalized content type, and canonical JSON body (sorted keys, normalized numbers, excluding transport headers). It transactionally inserts the key and resource/operation intent:

- the first request owns execution;
- a concurrent duplicate with the same hash waits for the committed result or receives retryable `409 IDEMPOTENCY_IN_PROGRESS` with `Retry-After`;
- a completed duplicate with the same hash replays the original status, selected response headers, and response body, without repeating side effects;
- reuse with a different hash returns non-retryable `409 IDEMPOTENCY_KEY_REUSED`.

Records and replay responses are retained for at least 24 hours from first acceptance. Retry after expiry may create a new side effect and SDKs warn when an operation's retry window has elapsed.

## Sandbox readiness and process state

Public sandbox states are:

```text
CREATING → SCHEDULED → STARTED → READY
READY → TERMINATING → FINISHED
any nonterminal state → FAILED
```

`READY` means the sandbox Pod is running, its runtime started, and (for `NVIDIA_L4`) GPU identity and health were attested by `ignition-gpu-agent`. There is no user-configured readiness probe; application health remains the application's responsibility. Exec is not yet servable — `ignition-gateway` is not built.

Process SDK handles are in this proto slice. The first CLI/SDK slice is `Sandbox` + `Process` + `Operation`.

Each Process follows:

```text
CREATING → STARTING → RUNNING → EXITED
                    ↘ CANCELLING → EXITED
any pre-terminal state → FAILED
```

`EXITED` includes exit code or terminating signal and immutable exit time. `FAILED` means the process could not be created/started or its terminal result was lost, and includes a typed reason. Client disconnect never changes process state. Cancellation sends a graceful termination signal, waits the declared grace period, then kills; repeated cancellation is idempotent.

## SDK and CLI contract

> The first Python and TypeScript SDK slice is implemented for sandbox,
> process, operation, and batch control-plane calls. `ignitionctl` and the exec
> byte stream are **not built**; process attachment currently returns gateway
> credentials only. The richer streaming behavior below remains the target
> contract.

SDK handles include `Project`, `Image`, `Secret`, `Sandbox`, `Process`, `Operation`, `Event`, `StreamReader`, and `StreamWriter`. No `Volume` or session-snapshot handle exists.

Python provides synchronous and native asynchronous APIs; TypeScript provides native promises and async iterators. Stream reads are bytes by default. Explicit text wrappers take an encoding and error policy, use an incremental decoder across frame boundaries, and expose partial final lines instead of dropping them. `iter_lines()` does not wait indefinitely for a newline and preserves whether a line was complete.

Writers expose bounded flow control: synchronous writes block up to a deadline and asynchronous writes await capacity. Readers do not buffer without limit. Cancellation propagates to pending calls without implicitly cancelling a remote Process unless requested. SDKs obtain a fresh Google ID token before control calls and a new stream credential for reconnect; they never reuse or refresh one credential class as another.

`Client`, `Sandbox`, and Process attachment support context managers/`using`. Exiting an attachment closes the local stream only. Exiting a client closes transports. Exiting a sandbox context terminates only if that context created the sandbox with `terminate_on_exit=True`.

```python
with Client() as client:
    sandbox = client.sandboxes.create(
        project="prj_...",
        image="img_...",
        command=["python", "-m", "server"],
        # resources/timeouts/network are optional; omitted fields come from
        # the system default runtime (CPU-only). Pass an accelerator for GPU:
        resources=Resources(accelerator=Accelerator(type="NVIDIA_L4", count=1)),
    )
    sandbox.wait_ready(timeout=120)
    process = sandbox.exec(["nvidia-smi"])
    for chunk in process.stdout.iter_bytes():
        consume(chunk)
    code = process.wait()
    sandbox.terminate(wait=True)
```

The CLI target: `gcloud`-based auth, project context, image import/status, secret lifecycle, sandbox create/list/inspect/terminate, exec/shell attach/reconnect, operation watch/cancel, and events, with PTY resize and detach, stable JSON output, explicit deadlines, secret redaction, and stable exit codes.

## Errors, quotas, and SLO

Errors include stable code, request ID, retryability, optional retry delay, and structured details. Quotas cover projects, active GPUs, creates, execs, streams, image/artifact bytes, process count, retained output bytes, and network connections.

For a `READY` sandbox, successful exec attachment has p95 latency at most 1 second, measured from the gateway receiving an authenticated attach request until the client receives the attached stream acknowledgement. The data-plane reconnect window is 10 minutes after process exit.

## Acceptance

- REST, Python, TypeScript, and CLI pass one black-box conformance suite over every public v1 resource and endpoint above.
- Schema and route tests prove Volume and SESSION snapshot resources/endpoints are absent from v1.
- Role-matrix tests cover every permission for user and service-account principals; a service account cannot be assigned as a role.
- Every endpoint returns indistinguishable `404` for nonexistent and cross-project IDs, and `403` for a known in-project denied action.
- Authorization queries cannot load an object before applying its project scope.
- Google ID tokens and Cloud IAP assertions with the correct issuer/`aud`/type authenticate; wrong issuer/audience/type, stale JWKS keys, excessive skew, unverified email, a disallowed hosted domain on a user token, and cross-class tokens fail closed with `401`.
- A subject with no `role_bindings` row (and no `domain:` fallback) is denied; a Google-side-disabled principal cannot obtain a new token.
- Concurrent same-hash idempotent requests produce one side effect and replay one response; different-hash key reuse conflicts; records remain replayable for 24 hours.
- Sandbox readiness succeeds without a user probe only after runtime/device/route setup. Every Process transition, cancel race, disconnect, exit code, signal, and startup failure is deterministic.
- SDK conformance covers binary data, split multibyte text, partial lines, bounded backpressure, local versus remote cancellation, token refresh, reconnect credentials, and context-manager cleanup.
- Operation/Event watch resumes after disconnect, reports retention gaps, and cancellation races have stable outcomes.
- Exec attach meets p95 1 second on a ready sandbox and reconnect remains available for 10 minutes after process exit.
