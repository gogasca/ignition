# Ignition

GPU sandboxes on GKE: one hostile tenant per one-L4 node, isolated with GKE Sandbox (gVisor / `nvproxy`).

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

| Binary | Role |
|---|---|
| `ignition-api` | Public REST API. Auth, admission, quota, idempotency. No Kubernetes access. |
| `ignition-controller` | Reconciles Cloud SQL desired state into GKE Sandbox Pods. Owns kube RBAC. |
| `ignition-gateway` | Exec data plane. Attach tokens from `ignition-api`; WebSocket stdin/stdout. |
| `ignitionctl` | CLI over the public API. |
| `sandbox-init` | Process supervisor that runs *inside* each gVisor sandbox. |

## Build

Requires Go 1.23+.

```bash
make build
make images IMAGE_REGISTRY=us-central1-docker.pkg.dev/PROJECT/ignition IMAGE_TAG=dev
```
