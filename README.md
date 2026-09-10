# Ignition

Ignition is a control plane for creating isolated sandboxes on GKE Standard with GKE Sandbox (`gvisor`/`nvproxy`) — the substrate for running **coding / tool-use agents** and **RL environments** (rollout workers for RL from verifiable rewards). A sandbox is CPU-only or one whole NVIDIA L4; the L4 path schedules one tenant sandbox per `g2-standard-8` node. `CreateSandbox` needs only an `imageId` — compute, timeouts, and networking default to a system-managed [default runtime](docs/design/ignition-shipped-architecture.md#6-default-runtime) (CPU-only) and can be overridden per request. The control plane owns auth, admission, quota, idempotency, lifecycle, and exec streaming; GKE owns compute; the trainer and inference for an RL run stay outside Ignition (see [`examples/agentic-rl/`](examples/agentic-rl/)).

Where this is going: [`docs/design/ROADMAP.md`](docs/design/ROADMAP.md). Architecture and public API contracts live in [`docs/design/`](docs/design/). **What is built vs not:** [`docs/design/STATUS.md`](docs/design/STATUS.md). The shipped system: [`docs/design/ignition-shipped-architecture.md`](docs/design/ignition-shipped-architecture.md). The public API: [`docs/design/ignition-api-contract.md`](docs/design/ignition-api-contract.md). Build images, create the cluster, and deploy: [`docs/guides/ignition-implementation.md`](docs/guides/ignition-implementation.md).

## Layout

```text
cmd/                  service and CLI entrypoints
internal/             private packages (not importable by SDKs)
api/proto/            public sandbox API (.proto)
api/openapi/          HTTP/JSON stub (kept in sync with protos)
internal/store/schema.sql  complete Cloud SQL schema (embedded by the API)
deploy/               GKE manifests and Terraform
images/sandbox-init/  container image for the in-sandbox supervisor
sdks/                 Python and TypeScript clients
examples/             worked SDK consumers (start with agentic-rl/)
docs/design/          architecture documents (start with STATUS.md)
docs/guides/          build and deploy runbook
```

## Examples

| Example | What it shows |
|---|---|
| [`examples/agentic-rl/`](examples/agentic-rl/) | Using a sandbox as the **environment / rollout worker** for RL from verifiable rewards: an LLM agent fixes a bug inside the sandbox, a pytest verifier scores it, trajectories flow back to a trainer. Two topologies, a hermetic test suite, and a real GRPO step (`trl.GRPOTrainer` via its `rollout_func` hook). Design notes: [`docs/design/agentic-rl-on-ignition.md`](docs/design/agentic-rl-on-ignition.md); runbook: [`docs/guides/agentic-rl-example.md`](docs/guides/agentic-rl-example.md). |

## Services

| Binary | Current status |
|---|---|
| `ignition-api` | Implemented HTTP/JSON API for sandbox, process, and operation state, plus a v0 image admission endpoint (`POST/GET /v1/projects/{project}/images`) that pins a client-given registry reference to a digest. Owns auth, admission, quota, and idempotency; has no Kubernetes RBAC. The image resolver does not yet restrict which registry host it will contact — see [Image delivery — Security status](docs/design/ignition-image-delivery.md#security-status). |
| `ignition-controller` | Implements the `STANDARD` GKE reconciliation path and is the only component with Pod/Node RBAC. `BARE_METAL` currently fails closed. |
| `sandbox-init` | In-sandbox liveness and accelerator readiness on port 8081 (`IGNITION_ACCELERATOR`: single-GPU check for `NVIDIA_L4`, supervisor-up for `NONE`), plus tenant-process supervision: reads desired processes from a projected file, runs/signals/reaps them (real PTY when `pty: true`), reports observed state + `idleSeconds` at `GET :8081/v1/processes`, and serves the exec byte stream at `GET :8081/v1/processes/{id}/attach`. |
| `ignition-gateway` | Implemented (`internal/gateway`): validates the exec-stream token, resolves the sandbox Pod by label, and proxies the attach WebSocket to `sandbox-init`. No product-database access; namespaced Pod read only. Every overlay deploys it; `staging`/`prod` add a public WebSocket `Ingress`. |
| `ignitionctl` | Implemented (`internal/cli`): `login` / `whoami` / context, sandbox + process + operation lifecycle, and `exec` with live stdio streaming through `ignition-gateway` (polling fallback when no gateway is configured). |

Python (`sdks/python`, no deps) and TypeScript (`sdks/typescript`, Node 22+) SDKs cover the same control-plane surface with exec streaming.

## Build

Requires Go 1.26.7 or a newer supported Go release. If present, the repository-local toolchain is `.tools/go/bin/go`.

```bash
make build GO=.tools/go/bin/go
make test GO=.tools/go/bin/go
make images IMAGE_REGISTRY=us-central1-docker.pkg.dev/PROJECT/ignition IMAGE_TAG=dev
(cd api/proto && buf lint)
```

`make images` builds only `ignition-api` and `ignition-controller`. The complete GCP prerequisites, sandbox image build, project-specific overlay rendering, deployment, API verification, and teardown commands are in the [implementation guide](docs/guides/ignition-implementation.md).

## Testing

Tests are split into three layers:

- Package unit tests cover domain logic and the in-memory store.
- `internal/store` tests execute the embedded schema and store queries against real PostgreSQL 16.
- `tests/integration` exercises API and controller workflows with the in-memory store and Kubernetes fake.

Run the complete suite with `make test`. The `internal/store` package uses Testcontainers to start
`postgres:16-alpine`, applies `internal/store/schema.sql`, runs the tests, and removes the container.
Docker must therefore be available when the store package is tested; inability to start PostgreSQL
is a test failure rather than a skipped test.

```bash
# Complete suite, including real PostgreSQL store tests.
make test GO=.tools/go/bin/go

# SQL and store behavior only.
.tools/go/bin/go test ./internal/store -count=1

# API/controller integration workflows only.
.tools/go/bin/go test ./tests/integration/... -count=1
```

CI or developers may provide an existing disposable PostgreSQL database instead of Docker by setting
`IGNITION_TEST_DATABASE_URL`. Tests create uniquely named project records, but the supplied database
must be dedicated to testing because the schema and test rows are not production-safe inputs.
