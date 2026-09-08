/**
 * Ignition TypeScript SDK — async client for the shipped control plane.
 *
 * ```ts
 * import { Client } from "@ignition/sandbox";
 *
 * const ignition = new Client();                 // server/token/project from env
 * const sb = await ignition.sandboxes.create("img_seed", { accelerator: "NONE", wait: true });
 * const result = await sb.run(["echo", "hello"]);
 * console.log(result.exitCode, new TextDecoder().decode(result.stdout));
 * await sb.terminate({ wait: true });
 * ```
 *
 * Config falls back to `IGNITION_SERVER` / `IGNITION_TOKEN` / `IGNITION_PROJECT`.
 * Scope: sandbox lifecycle, the process control plane, operations, `:watch`
 * streams, and exec streaming through `ignition-gateway` (Node 22+ global
 * WebSocket) with a polling fallback.
 */

export { Client, Sandbox, Process, Operation } from "./client.ts";
export type { CreateSandboxOptions, ExecOptions, RunOptions, ExecResult } from "./client.ts";
export type { TransportOptions } from "./http.ts";
export {
  IgnitionError,
  APIError,
  NotFoundError,
  PermissionDeniedError,
  UnauthenticatedError,
  ConflictError,
  TimeoutError,
  StreamError,
} from "./errors.ts";

export const VERSION = "0.1.0";
