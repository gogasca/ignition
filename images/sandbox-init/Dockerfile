# CPU sandbox-init seed image (accelerator: NONE).
#
# Minimal static image: readiness for a CPU sandbox is just the supervisor being
# up, so no nvidia-smi, CUDA, or libc are needed. The GPU variant lives in
# gpu.Dockerfile.
#   docker build -f images/sandbox-init/Dockerfile -t <repo>/img_seed_cpu:latest .
FROM golang:1.26.7-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/sandbox-init ./cmd/sandbox-init
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /sandbox-init ./cmd/sandbox-init

FROM gcr.io/distroless/static:nonroot
COPY --from=build /sandbox-init /ignition/init
ENTRYPOINT ["/ignition/init"]
