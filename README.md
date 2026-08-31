# Ignition

Ignition is an early implementation of isolated, one-GPU sandboxes on GKE Standard. The current dev path schedules one tenant sandbox per `g2-standard-8` node with one NVIDIA L4 and GKE Sandbox (`gvisor`/`nvproxy`).

Architecture and public API contracts live in [`docs/design/`](docs/design/). Start with [`docs/design/ignition-design-gke-sandbox.md`](docs/design/ignition-design-gke-sandbox.md). Software design for the API and controller: [`docs/design/ignition-design-api-controller.md`](docs/design/ignition-design-api-controller.md). Build images, create the cluster, and deploy: [`docs/guides/ignition-implementation.md`](docs/guides/ignition-implementation.md).

## Layout

```text
cmd/                  service and CLI entrypoints
internal/             private packages (not importable by SDKs)
api/proto/            public sandbox API (.proto)
api/openapi/          HTTP/JSON stub (kept in sync with protos)
db/migrations/        Cloud SQL schema
deploy/               GKE manifests and Terraform
images/sandbox-init/  container image for the in-sandbox supervisor
sdks/                 Python and TypeScript clients
docs/design/          architecture documents
docs/guides/          build and deploy runbook
```

## Services

| Binary | Current status |
|---|---|
| `ignition-api` | Implemented HTTP/JSON API for sandbox, process, and operation state. Owns auth, admission, quota, and idempotency; has no Kubernetes RBAC. |
| `ignition-controller` | Implements the `STANDARD` GKE reconciliation path and is the only component with Pod/Node RBAC. `BARE_METAL` currently fails closed. |
| `sandbox-init` | Implements in-sandbox liveness and single-GPU readiness on port 8081. Process supervision is not implemented yet. |
| `ignition-gateway` | Stub; exec-stream transport is not shipped. |
| `ignitionctl` | Stub; use the implementation guide's `curl` commands. |

## Build

Requires Go 1.26.7 or a newer supported Go release. If present, the repository-local toolchain is `.tools/go/bin/go`.

```bash
make build GO=.tools/go/bin/go
make test GO=.tools/go/bin/go
make images IMAGE_REGISTRY=us-central1-docker.pkg.dev/PROJECT/ignition IMAGE_TAG=dev
(cd api/proto && buf lint)
```

`make images` builds only `ignition-api` and `ignition-controller`. The complete GCP prerequisites, sandbox image build, project-specific overlay rendering, deployment, API verification, and teardown commands are in the [implementation guide](docs/guides/ignition-implementation.md).
