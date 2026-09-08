import { IgnitionError, StreamError, TimeoutError } from "./errors.ts";
import { Transport, type TransportOptions } from "./http.ts";

const SANDBOX_TERMINAL = new Set(["FINISHED", "FAILED"]);
const PROCESS_TERMINAL = new Set(["EXITED", "FAILED"]);
const OPERATION_TERMINAL = new Set(["SUCCEEDED", "FAILED", "CANCELLED"]);

const enc = new TextEncoder();
const dec = new TextDecoder();

export interface CreateSandboxOptions {
  command?: string[];
  workingDirectory?: string;
  nativeEntrypoint?: boolean;
  accelerator?: "NONE" | "NVIDIA_L4" | string;
  acceleratorCount?: number;
  cpuMilli?: number;
  memoryMiB?: number;
  internet?: boolean;
  env?: Record<string, string>;
  secretRefs?: Array<Record<string, unknown>>;
  labels?: Record<string, string>;
  region?: string;
  startupSeconds?: number;
  maximumRuntimeSeconds?: number;
  idleSeconds?: number;
  terminationGraceSeconds?: number;
  name?: string;
  idempotencyKey?: string;
  wait?: boolean;
  waitTimeoutMs?: number;
}

export interface ExecOptions {
  env?: Record<string, string>;
  workingDirectory?: string;
  pty?: boolean;
  ptyRows?: number;
  ptyCols?: number;
  idempotencyKey?: string;
}

export interface RunOptions extends ExecOptions {
  stdin?: Uint8Array | string;
  onStdout?: (chunk: Uint8Array) => void;
  onStderr?: (chunk: Uint8Array) => void;
  stream?: boolean;
  timeoutMs?: number;
}

export interface ExecResult {
  processId: string;
  exitCode: number | null;
  signal: string;
  stdout: Uint8Array;
  stderr: Uint8Array;
  get ok(): boolean;
}

export class Client {
  readonly t: Transport;
  readonly sandboxes: Sandboxes;
  readonly operations: Operations;

  constructor(opts: TransportOptions = {}) {
    this.t = new Transport(opts);
    this.sandboxes = new Sandboxes(this);
    this.operations = new Operations(this);
  }

  get project(): string {
    return this.t.project;
  }

  me(): Promise<{ subject: string; email: string; kind: string; domain: string }> {
    return this.t.get("/v1/me");
  }

  defaultRuntime(): Promise<Record<string, unknown>> {
    return this.t.get(this.t.projectPath("/runtimes/default"));
  }
}

class Sandboxes {
  private readonly c: Client;
  constructor(c: Client) {
    this.c = c;
  }

  async create(image: string, o: CreateSandboxOptions = {}): Promise<Sandbox> {
    const body: Record<string, unknown> = { imageId: image };
    if (o.name) body.name = o.name;
    if (o.command) body.command = o.command;
    if (o.workingDirectory) body.workingDirectory = o.workingDirectory;
    if (o.nativeEntrypoint) body.nativeEntrypoint = true;
    if (o.env) body.environment = o.env;
    if (o.secretRefs) body.secretRefs = o.secretRefs;
    if (o.labels) body.labels = o.labels;

    const resources: Record<string, unknown> = {};
    if (o.cpuMilli !== undefined) resources.cpuMilli = o.cpuMilli;
    if (o.memoryMiB !== undefined) resources.memoryMiB = o.memoryMiB;
    if (o.accelerator !== undefined) {
      const acc: Record<string, unknown> = { type: o.accelerator };
      if (o.acceleratorCount !== undefined) acc.count = o.acceleratorCount;
      else if (o.accelerator !== "NONE") acc.count = 1;
      resources.accelerator = acc;
    }
    if (Object.keys(resources).length) body.resources = resources;
    if (o.region) body.placement = { region: o.region };

    const timeouts: Record<string, number> = {};
    for (const [k, v] of [
      ["startupSeconds", o.startupSeconds],
      ["maximumRuntimeSeconds", o.maximumRuntimeSeconds],
      ["idleSeconds", o.idleSeconds],
      ["terminationGraceSeconds", o.terminationGraceSeconds],
    ] as const) {
      if (v !== undefined) timeouts[k] = v;
    }
    if (Object.keys(timeouts).length) body.timeouts = timeouts;
    body.network = { internetAccess: o.internet ? "ENABLED" : "DISABLED" };

    const resp = await this.c.t.post<{ sandbox: any }>(this.c.t.projectPath("/sandboxes"), body, {
      idempotent: true,
      idempotencyKey: o.idempotencyKey,
    });
    const sb = new Sandbox(this.c, resp.sandbox);
    if (o.wait) await sb.waitReady({ timeoutMs: o.waitTimeoutMs ?? 120_000 });
    return sb;
  }

  async get(id: string): Promise<Sandbox> {
    return new Sandbox(this.c, await this.c.t.get(this.c.t.projectPath(`/sandboxes/${id}`)));
  }

  async *list(pageSize?: number): AsyncGenerator<Sandbox> {
    let token = "";
    for (;;) {
      const resp = await this.c.t.get<{ sandboxes?: any[]; nextPageToken?: string }>(
        this.c.t.projectPath("/sandboxes"),
        { query: { pageSize: pageSize?.toString(), pageToken: token || undefined } },
      );
      for (const item of resp.sandboxes ?? []) yield new Sandbox(this.c, item);
      token = resp.nextPageToken ?? "";
      if (!token) return;
    }
  }
}

export class Sandbox {
  private readonly c: Client;
  raw: any;
  constructor(c: Client, raw: any) {
    this.c = c;
    this.raw = raw;
  }

  get id(): string {
    return this.raw.id ?? "";
  }
  get state(): string {
    return this.raw.state ?? "";
  }
  get stateReason(): string {
    return this.raw.stateReason ?? "";
  }
  get operationId(): string {
    return this.raw.operationId ?? "";
  }
  get isReady(): boolean {
    return this.state === "READY";
  }
  get isTerminal(): boolean {
    return SANDBOX_TERMINAL.has(this.state);
  }

  private path(suffix = ""): string {
    return this.c.t.projectPath(`/sandboxes/${this.id}${suffix}`);
  }

  async refresh(): Promise<this> {
    this.raw = await this.c.t.get(this.path());
    return this;
  }

  async *watch(lastEventId = ""): AsyncGenerator<this> {
    for await (const snap of this.c.t.sse(this.path(":watch"), lastEventId)) {
      this.raw = snap;
      yield this;
    }
  }

  async waitReady({ timeoutMs = 120_000 } = {}): Promise<this> {
    const deadline = Date.now() + timeoutMs;
    for await (const _ of pollOrWatch(this.c, this.path(":watch"), () => this.refresh(), deadline, (s) => {
      this.raw = s;
    })) {
      if (this.isReady) return this;
      if (this.isTerminal) {
        throw new IgnitionError(
          `sandbox ${this.id} became ${this.state} (${this.stateReason}) before READY`,
        );
      }
    }
    throw new TimeoutError(`sandbox ${this.id} not READY within ${timeoutMs}ms`);
  }

  async terminate({ wait = false, waitTimeoutMs = 60_000 } = {}): Promise<this> {
    const resp = await this.c.t.post<{ sandbox?: any }>(this.path(":terminate"), {}, { idempotent: true });
    this.raw = resp.sandbox ?? this.raw;
    if (wait) {
      const deadline = Date.now() + waitTimeoutMs;
      for await (const _ of pollOrWatch(this.c, this.path(":watch"), () => this.refresh(), deadline, (s) => {
        this.raw = s;
      })) {
        if (this.isTerminal) return this;
      }
      throw new TimeoutError(`sandbox ${this.id} not terminal within ${waitTimeoutMs}ms`);
    }
    return this;
  }

  get processes(): Processes {
    return new Processes(this.c, this.id);
  }

  exec(command: string[], o: ExecOptions = {}): Promise<Process> {
    return this.processes.create(command, o);
  }

  async run(command: string[], o: RunOptions = {}): Promise<ExecResult> {
    const proc = await this.exec(command, o);
    let attach: { gatewayUrl?: string; streamToken?: string } | undefined;
    if (o.stream !== false) {
      try {
        attach = await proc.attachToken();
      } catch {
        attach = undefined;
      }
    }
    if (attach?.gatewayUrl && attach?.streamToken) {
      return streamExec(attach.gatewayUrl, attach.streamToken, proc.id, o);
    }
    await proc.wait({ timeoutMs: o.timeoutMs });
    return {
      processId: proc.id,
      exitCode: proc.exitCode,
      signal: proc.signal,
      stdout: new Uint8Array(),
      stderr: new Uint8Array(),
      get ok() {
        return this.exitCode === 0;
      },
    };
  }
}

class Processes {
  private readonly c: Client;
  private readonly sandboxId: string;
  constructor(c: Client, sandboxId: string) {
    this.c = c;
    this.sandboxId = sandboxId;
  }

  private base(): string {
    return this.c.t.projectPath(`/sandboxes/${this.sandboxId}/processes`);
  }

  async create(command: string[], o: ExecOptions = {}): Promise<Process> {
    const body: Record<string, unknown> = { command };
    if (o.env) body.environment = o.env;
    if (o.workingDirectory) body.workingDirectory = o.workingDirectory;
    if (o.pty) {
      body.pty = true;
      if (o.ptyRows) body.ptyRows = o.ptyRows;
      if (o.ptyCols) body.ptyCols = o.ptyCols;
    }
    const raw = await this.c.t.post(this.base(), body, {
      idempotent: true,
      idempotencyKey: o.idempotencyKey,
    });
    return new Process(this.c, this.sandboxId, raw);
  }

  async get(id: string): Promise<Process> {
    return new Process(this.c, this.sandboxId, await this.c.t.get(`${this.base()}/${id}`));
  }

  async list(): Promise<Process[]> {
    const resp = await this.c.t.get<{ processes?: any[] }>(this.base());
    return (resp.processes ?? []).map((p) => new Process(this.c, this.sandboxId, p));
  }
}

export class Process {
  private readonly c: Client;
  readonly sandboxId: string;
  raw: any;
  constructor(c: Client, sandboxId: string, raw: any) {
    this.c = c;
    this.sandboxId = sandboxId;
    this.raw = raw;
  }

  get id(): string {
    return this.raw.id ?? "";
  }
  get state(): string {
    return this.raw.state ?? "";
  }
  get exitCode(): number | null {
    return this.raw.exitCode ?? null;
  }
  get signal(): string {
    return this.raw.signal ?? "";
  }
  get isTerminal(): boolean {
    return PROCESS_TERMINAL.has(this.state);
  }

  private path(suffix = ""): string {
    return this.c.t.projectPath(`/sandboxes/${this.sandboxId}/processes/${this.id}${suffix}`);
  }
  private attachPath(): string {
    return this.path(":attach");
  }

  async refresh(): Promise<this> {
    this.raw = await this.c.t.get(this.path());
    return this;
  }

  async signal(sig: string): Promise<this> {
    this.raw = await this.c.t.post(this.path(":signal"), { signal: sig }, { idempotent: true });
    return this;
  }

  async cancel(): Promise<this> {
    this.raw = await this.c.t.post(this.path(":cancel"), {}, { idempotent: true });
    return this;
  }

  attachToken(): Promise<{ streamToken: string; gatewayUrl: string; expireTime: string }> {
    return this.c.t.post(this.attachPath(), {}, { idempotent: true });
  }

  async stream(o: Omit<RunOptions, keyof ExecOptions> = {}): Promise<ExecResult> {
    const at = await this.attachToken();
    if (!at.gatewayUrl || !at.streamToken) {
      throw new StreamError("no gateway configured for this deployment");
    }
    return streamExec(at.gatewayUrl, at.streamToken, this.id, o);
  }

  async wait({ timeoutMs }: { timeoutMs?: number } = {}): Promise<number | null> {
    const deadline = Date.now() + (timeoutMs ?? 1e12);
    while (Date.now() < deadline) {
      await this.refresh();
      if (this.isTerminal) return this.exitCode;
      await sleep(500);
    }
    throw new TimeoutError(`process ${this.id} not terminal within ${timeoutMs}ms`);
  }
}

class Operations {
  private readonly c: Client;
  constructor(c: Client) {
    this.c = c;
  }

  async get(id: string): Promise<Operation> {
    return new Operation(this.c, await this.c.t.get(this.c.t.projectPath(`/operations/${id}`)));
  }

  async list(): Promise<Operation[]> {
    const resp = await this.c.t.get<{ operations?: any[] }>(this.c.t.projectPath("/operations"));
    return (resp.operations ?? []).map((o) => new Operation(this.c, o));
  }
}

export class Operation {
  private readonly c: Client;
  raw: any;
  constructor(c: Client, raw: any) {
    this.c = c;
    this.raw = raw;
  }

  get id(): string {
    return this.raw.id ?? "";
  }
  get state(): string {
    return this.raw.state ?? "";
  }
  get kind(): string {
    return this.raw.kind ?? "";
  }
  get resourceId(): string {
    return this.raw.resourceId ?? "";
  }
  get error(): Record<string, unknown> | null {
    return this.raw.error ?? null;
  }
  get isTerminal(): boolean {
    return OPERATION_TERMINAL.has(this.state);
  }
  get succeeded(): boolean {
    return this.state === "SUCCEEDED";
  }

  private path(suffix = ""): string {
    return this.c.t.projectPath(`/operations/${this.id}${suffix}`);
  }

  async refresh(): Promise<this> {
    this.raw = await this.c.t.get(this.path());
    return this;
  }

  async cancel(): Promise<this> {
    this.raw = await this.c.t.post(this.path(":cancel"), {}, { idempotent: true });
    return this;
  }

  async *watch(lastEventId = ""): AsyncGenerator<this> {
    for await (const snap of this.c.t.sse(this.path(":watch"), lastEventId)) {
      this.raw = snap;
      yield this;
    }
  }

  async wait({ timeoutMs = 300_000 } = {}): Promise<this> {
    const deadline = Date.now() + timeoutMs;
    for await (const _ of pollOrWatch(this.c, this.path(":watch"), () => this.refresh(), deadline, (s) => {
      this.raw = s;
    })) {
      if (this.isTerminal) return this;
    }
    throw new TimeoutError(`operation ${this.id} not terminal within ${timeoutMs}ms`);
  }
}

// -- shared helpers --------------------------------------------------
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function* pollOrWatch(
  c: Client,
  watchPath: string,
  refresh: () => Promise<unknown>,
  deadline: number,
  apply: (snap: any) => void,
): AsyncGenerator<void> {
  try {
    for await (const snap of c.t.sse(watchPath)) {
      apply(snap);
      yield;
      if (Date.now() > deadline) return;
    }
  } catch {
    /* fall through to polling */
  }
  while (Date.now() < deadline) {
    await refresh();
    yield;
    await sleep(1000);
  }
}

export async function streamExec(
  gatewayUrl: string,
  token: string,
  processId: string,
  o: { stdin?: Uint8Array | string; onStdout?: (c: Uint8Array) => void; onStderr?: (c: Uint8Array) => void } = {},
): Promise<ExecResult> {
  const wsUrl =
    gatewayUrl.replace(/^https:/, "wss:").replace(/^http:/, "ws:").replace(/\/+$/, "") +
    `/v1/attach?token=${encodeURIComponent(token)}`;

  const WS: typeof WebSocket = (globalThis as any).WebSocket;
  if (!WS) throw new StreamError("global WebSocket unavailable (Node 22+ required, or provide a polyfill)");

  const ws = new WS(wsUrl);
  const stdoutChunks: Uint8Array[] = [];
  const stderrChunks: Uint8Array[] = [];
  let exitCode: number | null = null;
  let signal = "";

  await new Promise<void>((resolve, reject) => {
    let settled = false;
    const done = () => {
      if (settled) return;
      settled = true;
      try {
        ws.close();
      } catch {
        /* already closing */
      }
      resolve();
    };
    const fail = (e: Error) => {
      if (settled) return;
      settled = true;
      reject(e);
    };

    ws.onerror = () => {
      // A close right after the exit frame surfaces as an error in some
      // runtimes; treat it as normal completion once we have the exit code.
      if (exitCode !== null || signal) done();
      else fail(new StreamError("gateway attach failed"));
    };
    ws.onopen = () => {
      const stdin = typeof o.stdin === "string" ? enc.encode(o.stdin) : o.stdin;
      if (stdin && stdin.length) {
        ws.send(JSON.stringify({ channel: "stdin", kind: "data", payload: toB64(stdin) }));
      }
      ws.send(JSON.stringify({ channel: "stdin", kind: "eof" }));
    };
    ws.onmessage = (ev: MessageEvent) => {
      const f = JSON.parse(typeof ev.data === "string" ? ev.data : dec.decode(ev.data as ArrayBuffer));
      const data = f.payload ? fromB64(f.payload) : new Uint8Array();
      if (f.channel === "stdout" && f.kind === "data") {
        stdoutChunks.push(data);
        o.onStdout?.(data);
      } else if (f.channel === "stderr" && f.kind === "data") {
        stderrChunks.push(data);
        o.onStderr?.(data);
      } else if (f.channel === "control" && f.kind === "exit") {
        exitCode = f.exitCode ?? null;
        signal = f.signal ?? "";
        done();
      } else if (f.kind === "error") {
        fail(new StreamError(`stream error: ${f.reason}`));
      }
    };
    ws.onclose = () => done();
  });

  return {
    processId,
    exitCode,
    signal,
    stdout: concat(stdoutChunks),
    stderr: concat(stderrChunks),
    get ok() {
      return this.exitCode === 0;
    },
  };
}

function toB64(u: Uint8Array): string {
  return Buffer.from(u).toString("base64");
}
function fromB64(s: string): Uint8Array {
  return new Uint8Array(Buffer.from(s, "base64"));
}
function concat(chunks: Uint8Array[]): Uint8Array {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.length;
  }
  return out;
}
