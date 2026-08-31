# Control-plane images

The control-plane Dockerfiles use a pinned `golang:1.26.7-bookworm` build stage and a non-root distroless runtime. Sandbox images stay under `images/sandbox-init/`; do not fold them into these files.

| File | Binary |
|---|---|
| `ignition-api.Dockerfile` | `cmd/ignition-api` (exposes 8080) |
| `ignition-controller.Dockerfile` | `cmd/ignition-controller` (no public port) |
| `ignition-gateway.Dockerfile` | `cmd/ignition-gateway` (exposes 8080). `internal/gateway` is a stub — the image builds but the binary exits non-zero at startup; no Deployment ships it yet. Built so the pipeline tracks its digest. |
| `ignition-prober.Dockerfile` | `cmd/ignition-prober` (exposes 9102). Runs the critical-user-journey probes against a live API; one-shot mode gates Cloud Deploy. Deployed by `deploy/k8s/components/prober`. |
| `control-plane.Dockerfile` | Compatibility Dockerfile for either binary via `--build-arg COMMAND=...`; the Makefile uses the dedicated files above |

```bash
make images IMAGE_REGISTRY=us-central1-docker.pkg.dev/PROJECT/ignition IMAGE_TAG="$(git rev-parse --short HEAD)"
```

Use `make push-images` with the same variables to rebuild and push all four images. CI builds the same
images with tests in front — see [`deploy/PIPELINE.md`](../PIPELINE.md). Build and deploy the
`sandbox-init` image separately by following the [implementation guide](../../docs/guides/ignition-implementation.md#5-images-rendered-overlay-and-control-plane-secret).
