# @ignition/sandbox (TypeScript)

Async client for the Ignition control plane. Zero dependencies — uses the
global `fetch` (Node 20+) and `WebSocket` (Node 22+).

```ts
import { Client } from "@ignition/sandbox";

const ignition = new Client(); // server/token/project from IGNITION_SERVER/TOKEN/PROJECT

const sb = await ignition.sandboxes.create("img_seed", {
  accelerator: "NONE",          // or "NVIDIA_L4"
  cpuMilli: 1000,
  memoryMiB: 2048,
  wait: true,                   // resolve once READY
});

const result = await sb.run(["echo", "hello"]); // streams stdio via ignition-gateway
console.log(result.exitCode, new TextDecoder().decode(result.stdout));

for await (const snap of sb.watch()) {          // SSE; break to stop
  if (snap.isTerminal) break;
}

await sb.terminate({ wait: true });
```

## Surface

| Object | Members |
|---|---|
| `Client` | `me()`, `defaultRuntime()`, `.sandboxes`, `.operations` |
| `client.sandboxes` | `create(image, opts)`, `get(id)`, `list()` (async iterator) |
| `Sandbox` | `waitReady()`, `watch()`, `terminate()`, `refresh()`, `exec(cmd)`, `run(cmd, opts)`, `.processes` |
| `sandbox.processes` | `create(cmd, opts)`, `get(id)`, `list()` |
| `Process` | `refresh()`, `signal(sig)`, `cancel()`, `wait()`, `attachToken()`, `stream(opts)` |
| `client.operations` | `get(id)`, `list()` |
| `Operation` | `refresh()`, `cancel()`, `watch()`, `wait()` |

Errors: `IgnitionError` base; `APIError` with `.code` / `.status` / `.requestId`;
`NotFoundError` (404), `PermissionDeniedError` (403), `UnauthenticatedError`
(401), `ConflictError` (409); `TimeoutError`, `StreamError`.

Every mutation sends an `Idempotency-Key`; pass `idempotencyKey` in the options
to pin it across retries.

## Not yet implemented

PTY resize, and the Project / Secret / Event resources (not exposed by the
server). `run(cmd, { stream: false })` returns the exit status but not captured
output.

## Dev

```
npm install
npm test          # node --test (runs the .ts files directly on Node 22+)
npm run typecheck  # tsc --noEmit
npm run build      # emits dist/
```
