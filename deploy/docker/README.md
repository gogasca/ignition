# Control-plane images

The control-plane Dockerfiles use a pinned `golang:1.26.7-bookworm` build stage and a non-root distroless runtime. Sandbox images stay under `images/sandbox-init/`; do not fold them into these files.

| File | Binary |
|---|---|
| `ignition-api.Dockerfile` | `cmd/ignition-api` (exposes 8080) |
| `ignition-controller.Dockerfile` | `cmd/ignition-controller` (no public port) |
| `control-plane.Dockerfile` | Compatibility Dockerfile for either binary via `--build-arg COMMAND=...`; the Makefile uses the dedicated files above |

```bash
make images IMAGE_REGISTRY=us-central1-docker.pkg.dev/PROJECT/ignition IMAGE_TAG="$(git rev-parse --short HEAD)"
```

Use `make push-images` with the same variables to rebuild and push both images. There is no `ignition-gateway` image yet. Build and deploy the `sandbox-init` image separately by following the [implementation guide](../../docs/guides/ignition-implementation.md#5-images-rendered-overlay-and-control-plane-secret).
