# syntax=docker/dockerfile:1
# Critical-user-journey prober. Runs the public API journeys against a live
# deployment and exports Prometheus metrics on :9102. With IGNITION_PROBE_ONESHOT
# it runs once and exits non-zero on failure (CI / Cloud Deploy verify gate).
#   docker build -f deploy/docker/ignition-prober.Dockerfile -t ignition-prober .

ARG COMMAND=ignition-prober

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
EXPOSE 9102
ENTRYPOINT ["/service"]
