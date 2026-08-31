# syntax=docker/dockerfile:1
# Data-plane exec/stream gateway.
#   docker build -f deploy/docker/ignition-gateway.Dockerfile -t ignition-gateway .
# NOTE: internal/gateway is currently a stub — the binary builds but exits
# non-zero at startup ("not implemented"). The image is produced so the pipeline
# tracks its digest; no Kubernetes Deployment ships it yet.

ARG COMMAND=ignition-gateway

FROM golang:1.26.7-bookworm AS build
ARG COMMAND
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/${COMMAND} ./cmd/${COMMAND}
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${COMMAND}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/service"]
