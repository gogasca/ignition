# syntax=docker/dockerfile:1
# Cluster reconciler. This is the only image that should receive Pod RBAC.
#   docker build -f deploy/docker/ignition-controller.Dockerfile -t ignition-controller .
# Not a public listener; do not expose it on an Ingress.

ARG COMMAND=ignition-controller

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
ENTRYPOINT ["/service"]
