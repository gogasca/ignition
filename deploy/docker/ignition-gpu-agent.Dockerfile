# syntax=docker/dockerfile:1
# ignition-gpu-agent: node-local trusted GPU authority. Runs as a DaemonSet on
# the GPU sandbox node pool. It execs nvidia-smi from the GKE driver install
# mounted in at /usr/local/nvidia (see deploy/k8s/components/gpu-agent), so the
# image itself carries no NVIDIA userspace.
#
# The runtime base is distroless/base (glibc), NOT static: the mounted
# nvidia-smi is a dynamically linked glibc binary (needs ld-linux,
# libc/libdl/libpthread/libm/librt) that also dlopens libnvidia-ml from
# /usr/local/nvidia/lib64. A static base has no dynamic loader, so nvidia-smi
# fails with "error while loading shared libraries: libdl.so.2" (or, before the
# driver install finishes, "no such file or directory" on the exec itself).
# Metrics on :9103.
#   docker build -f deploy/docker/ignition-gpu-agent.Dockerfile -t ignition-gpu-agent .
FROM golang:1.26.7-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ignition-gpu-agent ./cmd/ignition-gpu-agent
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/ignition-gpu-agent

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/service /service
USER nonroot:nonroot
EXPOSE 9103
ENTRYPOINT ["/service"]
