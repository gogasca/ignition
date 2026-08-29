# Control-plane images

Multi-stage `golang:1.23` → distroless static. Sandbox GPU images stay under `images/sandbox-init/`; do not fold them into these files.

| File | Binary |
|---|---|
| `ignition-api.Dockerfile` | `cmd/ignition-api` (exposes 8080) |
| `ignition-controller.Dockerfile` | `cmd/ignition-controller` (no public port) |
| `control-plane.Dockerfile` | either, via `--build-arg COMMAND=...` |

```bash
make images IMAGE_REGISTRY=us-central1-docker.pkg.dev/PROJECT/ignition IMAGE_TAG="$(git rev-parse --short HEAD)"
```

Deploy: [Implementation guide](../../docs/guides/ignition-implementation.md).
