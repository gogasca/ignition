# Ignition Client API and Identity Design

**Status:** Draft v0.2  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

## Scope

Defines the initial public v1 API, SDK and CLI contracts, external identity, project authorization, idempotency, errors, and compatibility. The initial production API does **not** expose persistent writable Volume resources or SESSION memory-snapshot resources; both are post-v1.

**Machine-readable schema:** [`api/proto/ignition/v1/`](../../api/proto/ignition/v1/) (`SandboxService`, `OperationService`). The first implementation slice is sandbox lifecycle, process exec, and operations. Project, image, secret, and event endpoints below remain specified but are **deferred** until their protos are added. See the [Implementation guide](../guides/ignition-implementation.md).

## Components

- `ignition-api`: resource CRUD, authentication, authorization, operations, and events.
- `ignitionctl`: human and automation CLI.
- Python package `ignition-sandbox`.
- TypeScript package `@ignition/sandbox`.
- A managed Auth0-compatible OpenID Connect provider. Ignition does not implement passwords, MFA, or primary identity storage.

## External authentication contract

Initial production uses these OAuth/OIDC flows:

- human browser and interactive CLI: Authorization Code with PKCE (`S256`);
- headless human CLI: Device Authorization Grant;
- machine-to-machine clients: Client Credentials with `private_key_jwt`. Shared client secrets and API keys are not general M2M credentials.

The provider issues RFC 9068 access JWTs with a 10-minute lifetime, asymmetric signatures, `typ=at+jwt`, stable subject, authorized-party/client ID, scopes, issuer, and the exact Ignition API audience. Its discovery document and HTTPS JWKS endpoint are part of the provider contract. Validators pin allowed algorithms, cache JWKS only within response bounds, refresh once on an unknown key ID, and fail closed if a usable key cannot be obtained. They allow at most 60 seconds of clock skew for `iat`, `nbf`, and `exp`.

Token validation requires exact issuer and audience, signature, allowed algorithm, expiry, not-before, token type, client authorization, and required scopes. ID tokens are never API credentials. Provider-side user/client disablement and refresh-token revocation stop future issuance; because access JWTs are self-contained, emergency revocation uses a control-plane denylist keyed by `jti` or subject/client through the remaining 10-minute lifetime.

API keys are one-time bootstrap/exchange credentials only. The API stores only a hash, binds each key to a principal and narrow exchange purpose, displays it once, expires it, and invalidates it on first successful exchange. API keys cannot call resource APIs or open data-plane connections.

Three credential classes are distinct and non-interchangeable:

1. external access JWTs for public control-plane API calls;
2. short-lived exec stream tokens bound to project, sandbox, process, stream epoch, and action;
3. internal route tokens plus SPIFFE mTLS identities for gateway-to-ingress routing.

No validator accepts a token issued for another class or audience. Internal workloads use workload identities represented by SPIFFE X.509 SVIDs and mTLS.

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

### Initial role/permission matrix

`owner` is immutable in the sense that a project must always retain at least one owner. `admin` can manage access but cannot transfer/delete the project or alter owners.

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

`own` means the principal initiated the sandbox or operation. Custom roles and resource-level sharing are post-v1. SQL-backed RBAC and explicit ownership/context checks are authoritative initially.

For an object ID that does not exist **or belongs to another project**, return `404 NOT_FOUND` with indistinguishable response shape and timing bounds. Return `403 PERMISSION_DENIED` when the object is in the requested project but the principal lacks **create** or **exec**. **Terminate and operation-cancel** deny on a known in-project object also return `404`, so existence is not leaked by the permission check.

## Public v1 resources and endpoints

All resource names are immutable opaque IDs. List APIs use cursor pagination and deterministic `(create_time, id)` ordering. Secret payloads are write-only and never returned.

The normative request, response, state progression, failure behavior, and acceptance tests for sandbox creation are defined in [Create Sandbox API Specification](ignition-sandbox-create-api.md). MVP provisioning behavior behind that contract (controller reconciliation, warm GPU node pools, and the warm-capacity startup SLO) is defined in the [GKE Sandbox MVP](ignition-design-gke-sandbox.md) design.

### First implementation slice (protos)

These endpoints are in `SandboxService` / `OperationService` today:

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
```

### Full v1 contract (deferred endpoints included)

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

Image import, sandbox creation/termination, secret rotation/deletion, and other long work return `202` with an Operation. Watch endpoints use authenticated Server-Sent Events. The implemented slice sends one snapshot plus heartbeats and closes after ~60s; `Last-Event-ID` and push-on-change remain the v1 contract. Clients recover with `GET`. Cancellation of an in-flight `CREATE_SANDBOX` fails the sandbox (`CANCELLED`) and releases quota; otherwise the Operation records whether work was cancelled or had already reached a non-cancellable/terminal point.

There are no Volume, sandbox volume-mount, or Session snapshot endpoints in initial production.

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

`READY` means runtime started and GPU/device setup succeeded, and ingress route registration has completed so the sandbox can accept exec. Initial production has no user-configured readiness probe; application health remains the application's responsibility.

Process SDK handles are in this proto slice. The first CLI/SDK slice is `Sandbox` + `Process` + `Operation`.

Each Process follows:

```text
CREATING → STARTING → RUNNING → EXITED
                    ↘ CANCELLING → EXITED
any pre-terminal state → FAILED
```

`EXITED` includes exit code or terminating signal and immutable exit time. `FAILED` means the process could not be created/started or its terminal result was lost, and includes a typed reason. Client disconnect never changes process state. Cancellation sends a graceful termination signal, waits the declared grace period, then kills; repeated cancellation is idempotent.

## SDK and CLI contract

SDK handles include `Project`, `Image`, `Secret`, `Sandbox`, `Process`, `Operation`, `Event`, `StreamReader`, and `StreamWriter`. No v1 `Volume` or session-snapshot handle exists.

Python provides synchronous and native asynchronous APIs; TypeScript provides native promises and async iterators. Stream reads are bytes by default. Explicit text wrappers take an encoding and error policy, use an incremental decoder across frame boundaries, and expose partial final lines instead of dropping them. `iter_lines()` does not wait indefinitely for a newline and preserves whether a line was complete.

Writers expose bounded flow control: synchronous writes block up to a deadline and asynchronous writes await capacity. Readers do not buffer without limit. Cancellation propagates to pending calls without implicitly cancelling a remote Process unless requested. SDKs refresh external access JWTs before control calls and obtain a new stream credential for reconnect; they never reuse or refresh one credential class as another.

`Client`, `Sandbox`, and Process attachment support context managers/`using`. Exiting an attachment closes the local stream only. Exiting a client closes transports. Exiting a sandbox context terminates only if that context created the sandbox with `terminate_on_exit=True`.

```python
with Client() as client:
    sandbox = client.sandboxes.create(
        project="prj_...",
        image="img_...",
        command=["python", "-m", "server"],
        gpu=GPU(type=GPUType.NVIDIA_L4, count=1),
        network=NetworkPolicy.deny_all(),
    )
    sandbox.wait_ready(timeout=120)
    process = sandbox.exec(["nvidia-smi"])
    for chunk in process.stdout.iter_bytes():
        consume(chunk)
    code = process.wait()
    sandbox.terminate(wait=True)
```

The CLI supports login/device login, project context, image import/status, secret lifecycle, sandbox create/list/inspect/terminate, exec/shell attach/reconnect, operation watch/cancel, and events. It supports PTY resize and detach, stable JSON output, explicit deadlines, secret redaction, and stable exit codes.

## Errors, quotas, and SLO

Errors include stable code, request ID, retryability, optional retry delay, and structured details. Quotas cover projects, active GPUs, creates, execs, streams, image/artifact bytes, process count, retained output bytes, and network connections.

For a `READY` sandbox, successful exec attachment has p95 latency at most 1 second, measured from the gateway receiving an authenticated attach request until the client receives the attached stream acknowledgement. The data-plane reconnect window is 10 minutes after process exit.

## Acceptance

- REST, Python, TypeScript, and CLI pass one black-box conformance suite over every public v1 resource and endpoint above.
- Schema and route tests prove Volume and SESSION snapshot resources/endpoints are absent from v1.
- Role-matrix tests cover every permission for user and service-account principals; a service account cannot be assigned as a role.
- Every endpoint returns indistinguishable `404` for nonexistent and cross-project IDs, and `403` for a known in-project denied action.
- Authorization queries cannot load an object before applying its project scope.
- Authorization Code + PKCE, Device Grant, and Client Credentials with `private_key_jwt` pass provider conformance; ID tokens, API keys, wrong issuer/audience/type, stale JWKS keys, excessive skew, and cross-class tokens fail closed.
- Disabled principals cannot obtain new tokens, emergency-denylisted access JWTs fail, and normal access JWTs expire within 10 minutes.
- Concurrent same-hash idempotent requests produce one side effect and replay one response; different-hash key reuse conflicts; records remain replayable for 24 hours.
- Sandbox readiness succeeds without a user probe only after runtime/device/route setup. Every Process transition, cancel race, disconnect, exit code, signal, and startup failure is deterministic.
- SDK conformance covers binary data, split multibyte text, partial lines, bounded backpressure, local versus remote cancellation, token refresh, reconnect credentials, and context-manager cleanup.
- Operation/Event watch resumes after disconnect, reports retention gaps, and cancellation races have stable outcomes.
- Exec attach meets p95 1 second on a ready sandbox and reconnect remains available for 10 minutes after process exit.
