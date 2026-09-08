import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { Server } from "node:http";
import { createHash } from "node:crypto";
import type { AddressInfo } from "node:net";

import { Client, NotFoundError, UnauthenticatedError } from "../src/index.ts";

const PROJECT = "prj_dev";
const state = { sandboxes: {} as Record<string, any>, procs: {} as Record<string, any>, hits: {} as Record<string, number> };
let gatewayURL = "";

function json(res: any, status: number, body: unknown) {
  const s = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json", "X-Request-Id": "req_test" });
  res.end(s);
}
function err(res: any, status: number, code: string) {
  json(res, status, { error: { code, message: code, requestId: "req_test" } });
}

let server: Server;
let base = "";

before(async () => {
  server = createServer((req, res) => {
    if (req.headers.authorization !== "Bearer t0ken") return err(res, 401, "UNAUTHENTICATED");
    const path = (req.url ?? "").split("?")[0].replace(`/v1/projects/${PROJECT}`, "/v1");

    if (req.method === "GET") {
      if (path === "/v1/me") return json(res, 200, { subject: "alice", email: "alice@acme.test", kind: "user" });
      if (path === "/v1/sandboxes")
        return json(res, 200, { sandboxes: Object.values(state.sandboxes), nextPageToken: "" });
      if (path.startsWith("/v1/sandboxes/") && path.endsWith(":watch")) {
        const id = path.slice("/v1/sandboxes/".length, -":watch".length);
        const sb = state.sandboxes[id];
        if (!sb) return err(res, 404, "NOT_FOUND");
        res.writeHead(200, { "Content-Type": "text/event-stream" });
        res.write(`id: 1\nevent: snapshot\ndata: ${JSON.stringify(sb)}\n\n`);
        sb.state = "READY";
        res.write(`id: 2\nevent: snapshot\ndata: ${JSON.stringify(sb)}\n\n`);
        res.end();
        return;
      }
      if (path.startsWith("/v1/sandboxes/") && path.includes("/processes/")) {
        const pid = path.split("/").pop()!;
        return state.procs[pid] ? json(res, 200, state.procs[pid]) : err(res, 404, "NOT_FOUND");
      }
      if (path.startsWith("/v1/sandboxes/") && path.endsWith("/processes"))
        return json(res, 200, { processes: Object.values(state.procs) });
      if (path.startsWith("/v1/sandboxes/")) {
        const id = path.slice("/v1/sandboxes/".length);
        const sb = state.sandboxes[id];
        if (!sb) return err(res, 404, "NOT_FOUND");
        state.hits[id] = (state.hits[id] ?? 0) + 1;
        if (state.hits[id] >= 2) sb.state = "READY";
        return json(res, 200, sb);
      }
      if (path === "/v1/operations")
        return json(res, 200, { operations: [{ id: "op_1", state: "SUCCEEDED", kind: "CREATE_SANDBOX" }], nextPageToken: "" });
      return err(res, 404, "NOT_FOUND");
    }

    // POST
    if (!req.headers["idempotency-key"]) return err(res, 400, "INVALID_ARGUMENT");
    if (path === "/v1/sandboxes") {
      const sb = { id: "sbx_1", projectId: PROJECT, state: "CREATING", imageId: "img_seed", operationId: "op_1" };
      state.sandboxes["sbx_1"] = sb;
      return json(res, 202, { sandbox: sb, operation: { id: "op_1", state: "PENDING", kind: "CREATE_SANDBOX" } });
    }
    if (path.endsWith(":terminate")) {
      const id = path.slice("/v1/sandboxes/".length, -":terminate".length);
      state.sandboxes[id].state = "FINISHED";
      return json(res, 202, { sandbox: state.sandboxes[id], operation: { id: "op_2", state: "SUCCEEDED" } });
    }
    if (path.endsWith("/processes")) {
      const pr = { id: "prc_1", sandboxId: "sbx_1", state: "RUNNING", command: ["echo", "hi"] };
      state.procs["prc_1"] = pr;
      return json(res, 200, pr);
    }
    if (path.endsWith(":attach"))
      return json(res, 200, { streamToken: "stok", gatewayUrl: gatewayURL, expireTime: "2030-01-01T00:00:00Z" });
    if (path.endsWith(":signal") || path.endsWith(":cancel")) {
      state.procs["prc_1"].state = "EXITED";
      state.procs["prc_1"].exitCode = 0;
      return json(res, 200, state.procs["prc_1"]);
    }
    return err(res, 404, "NOT_FOUND");
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

after(() => server.close());

const client = () => new Client({ server: base, token: "t0ken", project: PROJECT });

test("me()", async () => {
  assert.equal((await client().me()).subject, "alice");
});

test("auth error", async () => {
  await assert.rejects(new Client({ server: base, token: "bad", project: PROJECT }).me(), UnauthenticatedError);
});

test("not found", async () => {
  await assert.rejects(client().sandboxes.get("sbx_missing"), NotFoundError);
});

test("create / list / terminate", async () => {
  const c = client();
  const sb = await c.sandboxes.create("img_seed", { accelerator: "NONE", cpuMilli: 1000, memoryMiB: 2048 });
  assert.equal(sb.state, "CREATING");
  assert.equal(sb.id, "sbx_1");
  const ids: string[] = [];
  for await (const s of c.sandboxes.list()) ids.push(s.id);
  assert.ok(ids.includes("sbx_1"));
  await sb.terminate();
  assert.ok(sb.isTerminal);
});

test("waitReady via watch", async () => {
  const sb = await client().sandboxes.create("img_seed", { accelerator: "NONE" });
  await sb.waitReady({ timeoutMs: 5000 });
  assert.ok(sb.isReady);
});

test("process lifecycle", async () => {
  const sb = await client().sandboxes.create("img_seed");
  const proc = await sb.exec(["echo", "hi"]);
  assert.equal(proc.state, "RUNNING");
  await proc.cancel();
  assert.ok(proc.isTerminal);
  assert.equal(proc.exitCode, 0);
});

// -- exec streaming against a raw-TCP fake gateway --------------------
test("run() streams through the gateway", async () => {
  const wsServer = createServer();
  const sockets: import("node:net").Socket[] = [];
  let gotClientFrame = false;
  wsServer.on("upgrade", (req, socket) => {
    sockets.push(socket);
    const key = req.headers["sec-websocket-key"]!;
    const accept = createHash("sha1")
      .update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
      .digest("base64");
    socket.write(
      "HTTP/1.1 101 Switching Protocols\r\n" +
        "Upgrade: websocket\r\nConnection: Upgrade\r\n" +
        `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
    );
    const textFrame = (s: string) => {
      const p = Buffer.from(s);
      return Buffer.concat([Buffer.from([0x81, p.length]), p]);
    };
    socket.on("data", () => {
      gotClientFrame = true;
    });
    // Reply with stdout + exit, then close the TCP so the client's WebSocket
    // sees the disconnect. The delay lets the client's stdin frames land first.
    setTimeout(() => {
      socket.write(
        textFrame(JSON.stringify({ channel: "stdout", kind: "data", payload: Buffer.from("hi\n").toString("base64") })),
      );
      socket.write(textFrame(JSON.stringify({ channel: "control", kind: "exit", exitCode: 5 })));
      socket.write(Buffer.from([0x88, 0x00]));
      socket.end();
    }, 40);
  });
  await new Promise<void>((r) => wsServer.listen(0, "127.0.0.1", r));
  gatewayURL = `http://127.0.0.1:${(wsServer.address() as AddressInfo).port}`;

  try {
    const sb = await client().sandboxes.create("img_seed");
    const result = await sb.run(["echo", "hi"], { stdin: "ping\n" });
    assert.equal(result.exitCode, 5);
    assert.equal(new TextDecoder().decode(result.stdout), "hi\n");
    assert.ok(gotClientFrame, "gateway received client frames");
  } finally {
    for (const s of sockets) s.destroy();
    wsServer.close();
  }
});
