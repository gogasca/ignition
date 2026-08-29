FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/sandbox-init ./cmd/sandbox-init
COPY internal/sandboxinit ./internal/sandboxinit
RUN CGO_ENABLED=0 go build -o /sandbox-init ./cmd/sandbox-init

FROM gcr.io/distroless/static:nonroot
COPY --from=build /sandbox-init /ignition/init
ENTRYPOINT ["/ignition/init"]
