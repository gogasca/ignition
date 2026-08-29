# Protobuf contracts for the public Ignition sandbox API (`ignition.v1`).
#
# Canonical behavior remains in docs/design/ignition-sandbox-create-api.md
# and docs/design/ignition-design-client-api-identity.md. These protos are
# the machine-readable schema for SDKs, ignitionctl, and ignition-api.
#
# Layout
#   ignition/v1/sandbox_service.proto  SandboxService (lifecycle + process RPCs)
#   ignition/v1/sandbox.proto          Sandbox resource + create/list messages
#   ignition/v1/process.proto          Process resource + exec frames
#   ignition/v1/operation.proto        Operation + OperationService
#   ignition/v1/common.proto           pagination, Status, ErrorCode
#
# REST paths are documented on each RPC. v1 public transport is HTTP/JSON
# (and SSE for watch); gRPC may be offered internally. Exec stdin/stdout
# bytes travel on ignition-gateway using AttachProcess tokens, not this
# control-plane service.
#
# Generate (requires buf). `buf generate` needs `buf.gen.yaml` (not in the repo yet).
#   buf lint
