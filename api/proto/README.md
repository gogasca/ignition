# Ignition v1 protobuf contracts

These files are the machine-readable schema for the public Ignition sandbox API. Canonical behavior is defined by the [Create Sandbox API](../../docs/design/ignition-sandbox-create-api.md) and [client API and identity design](../../docs/design/ignition-design-client-api-identity.md).

## Layout

| File | Contents |
|---|---|
| `ignition/v1/sandbox_service.proto` | `SandboxService` lifecycle and process RPCs |
| `ignition/v1/sandbox.proto` | Sandbox resource and create/list messages |
| `ignition/v1/process.proto` | Process resource and exec-frame schema |
| `ignition/v1/operation.proto` | Operation resource and `OperationService` |
| `ignition/v1/common.proto` | Pagination, status, and error types |

The public v1 transport is HTTP/JSON, with SSE for watch endpoints. RPC comments record the REST paths. Idempotency is carried only in the HTTP `Idempotency-Key` header, not in protobuf request messages. The gateway transport for stdin/stdout is specified by `ExecFrame`, but `ignition-gateway` is not implemented yet.

## Validate

From this directory:

```bash
buf lint
buf build >/dev/null
```

The module uses `STANDARD` lint with documented exceptions for intentional AIP-style resource responses. Breaking-change detection uses `WIRE_JSON` compatibility.

`buf generate` is not available because the repository does not yet contain `buf.gen.yaml`; generated Go bindings are not checked in. Do not hand-write files under a future `api/gen/` tree.
