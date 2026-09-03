# CPU sandbox-init seed image (accelerator: NONE).
#
# Minimal static image: readiness for a CPU sandbox is just the supervisor being
# up, so no nvidia-smi, CUDA, or libc are needed. The GPU variant lives in
# gpu.Dockerfile.
#   docker build -f images/sandbox-init/Dockerfile -t <repo>/img_seed_cpu:latest .
FROM golang:1.26.7-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/sandbox-init ./cmd/sandbox-init
COPY internal/sandboxinit ./internal/sandboxinit
COPY internal/gpuid ./internal/gpuid
RUN CGO_ENABLED=0 go build -o /sandbox-init ./cmd/sandbox-init

FROM gcr.io/distroless/static:nonroot
COPY --from=build /sandbox-init /ignition/init
ENTRYPOINT ["/ignition/init"]
