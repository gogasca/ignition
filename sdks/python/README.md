# ignition-sandbox (Python)

Synchronous client for the Ignition control plane. No runtime dependencies —
the exec stream uses a built-in minimal WebSocket client.

```python
from ignition_sandbox import Client

with Client() as ignition:  # server/token/project from IGNITION_SERVER/TOKEN/PROJECT
    sb = ignition.sandboxes.create(
        "img_seed",
        accelerator="NONE",        # or "NVIDIA_L4"
        cpu_milli=1000, memory_mib=2048,
        wait=True,                 # block until READY
    )
    result = sb.run(["echo", "hello"])   # streams stdio through ignition-gateway
    print(result.exit_code)

    for snapshot in sb.watch():          # SSE, stops when you break
        if snapshot.is_terminal:
            break

    sb.terminate(wait=True)
```

## Surface

| Object | Methods |
|---|---|
| `Client` | `me()`, `default_runtime()`, `.sandboxes`, `.operations` |
| `Client.sandboxes` | `create(...)`, `get(id)`, `list()` |
| `Sandbox` | `wait_ready()`, `watch()`, `terminate()`, `refresh()`, `exec(cmd)`, `run(cmd, ...)`, `.processes` |
| `Sandbox.processes` | `create(cmd, ...)`, `get(id)`, `list()` |
| `Process` | `refresh()`, `signal(sig)`, `cancel()`, `wait()`, `attach_token()`, `stream(...)` |
| `Client.operations` | `get(id)`, `list()` |
| `Operation` | `refresh()`, `cancel()`, `watch()`, `wait()` |

Errors: `IgnitionError` base; `APIError` with `.code` / `.status` / `.request_id`;
`NotFoundError` (404), `PermissionDeniedError` (403), `UnauthenticatedError`
(401), `ConflictError` (409); `TimeoutError`, `StreamError`.

Every mutation sends an `Idempotency-Key` automatically; pass
`idempotency_key=...` to pin it across retries.

## Not yet implemented

A native `async` client, PTY resize, and the Project / Secret / Event resources
(not exposed by the server). `run(..., stream=False)` returns the exit status
but not captured output — the API has no output-read endpoint outside the
gateway stream.

## Tests

```
pip install -e ".[test]"
pytest
```
