# GPU sandbox-init seed image (accelerator: NVIDIA_L4).
#
# Built FROM an NVIDIA CUDA base so that:
#   - the node-injected, dynamically-linked nvidia-smi runs (glibc present), and
#   - cuda-check can dlopen libcuda.so.1 (injected by the GKE GPU device plugin)
#     for a real cuInit() as the "CUDA works" half of the readiness probe.
#
# The CUDA base pins a toolkit version — keep it at or below what the GKE L4
# `default` driver supports and rely on CUDA minor-version compatibility. Mirror
# the base into the same-region Artifact Registry (image admission requires it):
#   CUDA_BASE=<region>-docker.pkg.dev/<project>/mirror/nvidia/cuda:12.4.1-base-ubuntu22.04
#   docker build -f images/sandbox-init/gpu.Dockerfile \
#     --build-arg CUDA_BASE="$CUDA_BASE" -t <repo>/img_seed_gpu:latest .
ARG CUDA_BASE=nvidia/cuda:12.4.1-base-ubuntu22.04

FROM golang:1.26.7-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/sandbox-init ./cmd/sandbox-init
COPY cmd/cuda-check ./cmd/cuda-check
COPY internal/sandboxinit ./internal/sandboxinit
COPY internal/gpuid ./internal/gpuid
RUN CGO_ENABLED=0 go build -o /out/init ./cmd/sandbox-init
RUN CGO_ENABLED=1 go build -o /out/cuda-check ./cmd/cuda-check

FROM ${CUDA_BASE}
COPY --from=build /out/init /ignition/init
COPY --from=build /out/cuda-check /ignition/cuda-check
# libcuda.so.1, nvidia-smi and friends are mounted in at runtime by the device
# plugin; nothing CUDA-runtime-related is baked here beyond the base image.
USER 65532:65532
ENTRYPOINT ["/ignition/init"]
