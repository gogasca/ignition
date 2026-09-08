/** Fetch-based transport. No dependencies: Node 20+ has global fetch, Node 22+
 * has global WebSocket (used by the exec stream). */

import { IgnitionError, errorFromResponse } from "./errors.ts";

const USER_AGENT = "ignition-sandbox-ts/0.1";

export interface TransportOptions {
  server?: string;
  token?: string;
  project?: string;
  timeoutMs?: number;
}

function env(name: string): string {
  return (globalThis as any).process?.env?.[name] ?? "";
}

export class Transport {
  readonly server: string;
  readonly token: string;
  readonly project: string;
  readonly timeoutMs: number;

  constructor(opts: TransportOptions) {
    this.server = (opts.server || env("IGNITION_SERVER")).replace(/\/+$/, "");
    this.token = opts.token || env("IGNITION_TOKEN");
    this.project = opts.project || env("IGNITION_PROJECT");
    this.timeoutMs = opts.timeoutMs ?? 30_000;
    if (!this.server) {
      throw new IgnitionError("no server configured: pass server or set IGNITION_SERVER");
    }
  }

  projectPath(suffix: string): string {
    if (!this.project) {
      throw new IgnitionError("no project configured: pass project or set IGNITION_PROJECT");
    }
    return `/v1/projects/${this.project}${suffix}`;
  }

  private headers(extra: Record<string, string> = {}): Record<string, string> {
    const h: Record<string, string> = { "User-Agent": USER_AGENT, ...extra };
    if (this.token) h.Authorization = `Bearer ${this.token}`;
    return h;
  }

  async request<T = unknown>(
    method: string,
    path: string,
    opts: {
      body?: unknown;
      idempotent?: boolean;
      idempotencyKey?: string;
      query?: Record<string, string | undefined>;
    } = {},
  ): Promise<T> {
    let url = this.server + path;
    if (opts.query) {
      const qs = Object.entries(opts.query)
        .filter(([, v]) => v !== undefined && v !== "")
        .map(([k, v]) => `${k}=${encodeURIComponent(v as string)}`)
        .join("&");
      if (qs) url += `?${qs}`;
    }

    const headers = this.headers({ Accept: "application/json" });
    let body: string | undefined;
    if (opts.body !== undefined) {
      body = JSON.stringify(opts.body);
      headers["Content-Type"] = "application/json";
    }
    if (opts.idempotent) {
      headers["Idempotency-Key"] = opts.idempotencyKey || `tsdk-${crypto.randomUUID()}`;
    }

    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), this.timeoutMs);
    let resp: Response;
    try {
      resp = await fetch(url, { method, headers, body, signal: ac.signal });
    } catch (e) {
      throw new IgnitionError(`${method} ${path}: ${(e as Error).message}`);
    } finally {
      clearTimeout(timer);
    }

    const text = await resp.text();
    if (!resp.ok) {
      throw errorFromResponse(resp.status, text, resp.headers.get("x-request-id") ?? "");
    }
    return (text ? JSON.parse(text) : undefined) as T;
  }

  get<T = unknown>(path: string, opts?: Parameters<Transport["request"]>[2]): Promise<T> {
    return this.request<T>("GET", path, opts);
  }

  post<T = unknown>(path: string, body?: unknown, opts: Parameters<Transport["request"]>[2] = {}): Promise<T> {
    return this.request<T>("POST", path, { ...opts, body });
  }

  /** Yield each JSON snapshot from an SSE `:watch` stream. */
  async *sse(path: string, lastEventId = ""): AsyncGenerator<any> {
    const headers = this.headers({ Accept: "text/event-stream" });
    if (lastEventId) headers["Last-Event-ID"] = lastEventId;
    const resp = await fetch(this.server + path, { method: "GET", headers });
    if (!resp.ok) {
      throw errorFromResponse(resp.status, await resp.text(), resp.headers.get("x-request-id") ?? "");
    }
    const reader = resp.body!.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;
        buf += decoder.decode(value, { stream: true });
        let nl: number;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl).replace(/\r$/, "");
          buf = buf.slice(nl + 1);
          if (line.startsWith("data: ")) {
            try {
              yield JSON.parse(line.slice(6));
            } catch {
              /* ignore keepalive / partial */
            }
          }
        }
      }
    } finally {
      try {
        await reader.cancel();
      } catch {
        /* already closed */
      }
    }
  }
}
