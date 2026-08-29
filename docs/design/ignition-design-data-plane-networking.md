# Ignition Data Plane and Networking Design

**Status:** Draft v0.2 — target architecture; not the current deploy path  
**Parent:** [Ignition Technical Design](ignition-technical-design.md)

> **Implementation status.** This document specifies the full data plane for the
> custom GCE worker runtime: `ignition-ingress` as a per-worker proxy,
> `worker-control` as the sole route-state writer, a Postgres route table with a
> transactional outbox, SPIFFE mTLS, and a Local-SSD exec spool. **None of that
> is built.** The [GKE Sandbox MVP](ignition-design-gke-sandbox.md) collapses the
> data plane to `ignition-gateway` validating an `ignition-api`-minted attach
> token and proxying a WebSocket to the sandbox init supervisor over the Pod
> network; there is no `ignition-ingress`, no route table, and no outbox. In the
> current code `ignition-gateway` and `sandbox-init` are stubs and neither is
> deployed — see the [Implementation guide](../guides/ignition-implementation.md).
> The egress policy, credential-audience separation, and attach-token shape in
> this document are runtime-agnostic and do apply to the MVP.

## Scope

Defines `ignition-gateway`, `ignition-ingress`, authoritative routing, exec reconnect, readiness, VPC boundaries, egress controls, draining, and availability.

This module covers `ignition-gateway` and exec attach. See the [Implementation guide](../guides/ignition-implementation.md) for what the first slice actually deploys (`ignition-api` and `ignition-controller` only).

## Components and network boundary

- `ignition-gateway`: multi-zone GKE data-plane service behind an external Application Load Balancer.
- `ignition-ingress`: per-worker proxy and bounded Local-SSD exec spool owner.
- `worker-control`: the only writer of sandbox route lifecycle updates.
- gVisor netstack plus worker host firewall: sandbox network boundary.

```text
client
→ external Application Load Balancer (public TLS)
→ ignition-gateway (external access/exec credential validation)
→ SPIFFE mTLS over private RFC1918 address
→ ignition-ingress (internal route-token validation)
→ gVisor netstack
→ sandbox process
```

Gateway and workers share a VPC but use separate private subnets and least-privilege firewall identities. Workers have RFC1918 addresses and no public IP. Firewall rules allow gateway identities to reach only the ingress listener, worker-control to reach only its management listener, and workers to use approved egress proxies/services. Clients never receive a worker address. Control APIs do not proxy long-running exec payloads.

## Authoritative route table

Postgres is authoritative for a route keyed by `(sandbox_id, generation)`. Each row contains:

- project ID and sandbox ID;
- monotonically increasing sandbox generation;
- target worker SPIFFE ID;
- private RFC1918 ingress endpoint;
- monotonically increasing ingress epoch, changed whenever ingress/spool identity is replaced;
- route state: `STARTING`, `READY`, `DRAINING`, or `REMOVED`;
- update version and timestamps.

`worker-control` transactionally updates sandbox state, the generation-specific route row, and an outbox event in one Postgres commit. No component publishes a ready route before that commit. Gateways consume the outbox into a bounded local cache and can read through to Postgres after a cache miss or outbox gap. Every request compares its credential generation to the cached row and, before opening a backend connection, validates that the selected row is current and `READY`. Stale generation, ingress epoch, worker identity, endpoint, or removed/draining state fails closed. A cache entry can accelerate lookup but never override Postgres generation.

The gateway mints a short-lived internal route token after validating the external credential. It includes project, sandbox, generation, ingress epoch, optional process ID, action, protocol, internal audience, expiry, and unique token ID. It cannot contain a client-selected IP or worker port. `ignition-ingress` requires both a route token and gateway SPIFFE identity, then validates their audience, signature, expiry, generation, ingress epoch, target worker identity, and current local sandbox state.

External access JWTs, exec stream tokens, and internal route tokens use separate audiences, signing contexts, schemas, and validators.

## Exec transport and reconnect

Exec attachment uses authenticated WebSocket in initial production. End-to-end gRPC/HTTP/2 is deferred. Process lifetime is independent of gateway or client connection lifetime.

`ignition-ingress`, not the gateway, owns a per-process bounded spool on worker Local SSD. The spool remains active across gateway disconnects/restarts and retains stdout/stderr through the reconnect window. It is scoped to `(sandbox_id, generation, process_id, stream_epoch)`, quota-accounted, encrypted by the node storage boundary, and deleted after expiry. Worker or Local-SSD loss may destroy it and is reported as a gap, never as complete replay.

Each binary frame has this logical schema:

```text
version
sandbox_id, generation, process_id, stream_epoch
channel                 # STDIN | STDOUT | STDERR | CONTROL
kind                    # DATA | ACK | EOF | EXIT | TRUNCATED | GAP |
                        # RESIZE_PTY | SIGNAL | HEARTBEAT | ERROR
byte_start, byte_end    # half-open channel byte range; equal for control frames
ack_through             # next byte expected, for ACK
payload
exit_code, signal       # EXIT only
reason                  # TRUNCATED, GAP, or ERROR
```

Within a stream epoch, DATA ranges for each byte channel are contiguous and never overlap. ACK is cumulative and means all bytes before `ack_through` were durably consumed by that side. EOF is channel-specific and is emitted only after all preceding bytes. EXIT is emitted after stdout/stderr EOF and is replayable. A new stream epoch is created when ingress cannot prove continuity; offsets restart at zero and the first frame identifies the prior terminal offset with `GAP`.

On attach/reconnect the client presents a newly minted exec stream token and its last acknowledged stdout/stderr offsets, stdin acknowledged offset, stream epoch, and observed terminal state. Ingress replies with accepted offsets or explicit `TRUNCATED`/`GAP`, then replays stdout/stderr. The reconnect window ends 10 minutes after process exit; while a process is running, retained bytes remain subject to the same bounded spool quota.

### Input and output safety

- stdout and stderr are replayable from acknowledged byte offsets.
- stdin DATA has a byte sequence and cumulative ACK. A client may replay only a range known not to have been acknowledged. If the last stdin ACK is unknown after disconnect, the SDK does **not** automatically replay; it reports an indeterminate-write result and requires the caller to choose.
- stdin EOF is sequenced and acknowledged. Duplicate known frames are discarded; overlapping or conflicting bytes terminate the attachment.
- flow control is per channel. Gateway and ingress memory buffers are bounded independently of disk spool.
- when an output quota is reached, ingress drops the oldest fully framed stdout/stderr ranges, emits and persists `TRUNCATED` with the first retained offset, and continues the process. A reconnect requesting older bytes receives `TRUNCATED`; silent loss is forbidden.
- a currently connected slow reader receives backpressure until the memory/disk high-water limit, then receives `TRUNCATED` and the oldest output is discarded. Tenant traffic cannot exhaust service memory or Local SSD.
- non-PTY mode has independent stdin, stdout, and stderr channels and half-closes.
- PTY mode has one merged output channel; stderr is not separately recoverable. Client stdin EOF performs a terminal input half-close without closing output. Detach never sends EOF, SIGHUP, or termination. PTY output remains open until process exit, and resize/signal controls are ordered but not replayed as byte data.

## Readiness

The worker reports a sandbox `READY` only after runtime start, GPU/device visibility, ingress local registration, and the Postgres route/outbox transaction commits. Gateway independently validates generation and route state. Initial production has no user application readiness probe; it does not infer that a user application is healthy.

In the MVP there is no ingress registration or route/outbox commit: `ignition-controller` marks a sandbox `READY` when the Pod is scheduled and running, the init supervisor has set `ignition.io/init-healthy=true`, and exactly one GPU UUID is annotated (`ignition.io/gpu-uuid`). See the state-mapping table in [API and Controller proposal](ignition-design-api-controller.md) §6.2.

## Egress policy

Default egress is deny all. CIDR allowlists and domain allowlists are separate policy types.

A CIDR policy accepts normalized IPv4 and IPv6 prefixes but always subtracts metadata, loopback, link-local, multicast, worker, sandbox, gateway/control-plane, and private management ranges. An allowed direct IP must match a CIDR rule; domain permission never authorizes direct-IP connection.

A domain policy permits TLS only on destination port 443 through the platform L7 CONNECT/SNI proxy:

- names are lower-cased IDNA A-labels with no trailing dot;
- an exact rule matches only that name;
- `*.example.com` matches exactly one or more subdomain labels, never the apex `example.com`;
- DNS resolution follows at most eight CNAMEs, and every CNAME target plus final name must satisfy the same allowlist;
- A and AAAA answers are filtered through the blocked-range set and pinned to the authorized connection;
- TLS SNI is mandatory, must match the authorized normalized domain, and the proxy verifies certificate hostname and chain;
- IPv4 and IPv6 use identical checks.

Sandbox access to external DNS is redirected to the platform resolver. Direct DNS, DNS-over-HTTPS, DNS-over-TLS, QUIC/UDP 443, ECH that hides the authorized SNI, proxy chaining, and direct-IP bypass are blocked initially. DNS rebinding cannot change the pinned connect destination. Approved private-service endpoints require a separate explicit policy.

Always block metadata services, worker management sockets, control-plane endpoints, Local-SSD/cache services, other sandbox addresses, and the egress proxy's own administration endpoints.

## Drain and failure behavior

1. `worker-control` commits route state `DRAINING` and an outbox event.
2. Gateways reject new exec connections for that generation.
3. Ingress allows attached exec streams to drain.
4. Remaining exec attachments receive a retryable termination; processes continue or terminate according to the control-plane action.
5. `worker-control` commits `REMOVED`, then worker cleanup proceeds.

Gateway restart does not affect ingress-owned exec replay. Ingress/worker loss increments ingress epoch, invalidates the old route, and causes an explicit exec `GAP` if the process can otherwise be recovered. No component claims replay continuity it cannot prove.

## Availability, security, and observability

- Gateway service availability SLO is 99.9% per calendar month, measured as valid data-plane requests not returning gateway-attributable 5xx/unavailability; tenant errors and unavailable/terminating sandboxes are excluded.
- Successful exec attach to a `READY` sandbox is p95 at most 1 second from authenticated gateway receipt to attachment acknowledgement.
- The reconnect window is 10 minutes after process exit.
- Gateway replicas span zones and deploy with connection draining; worker ingress failure makes the worker unschedulable.
- Rate limits apply by project, sandbox, process, and source.
- No command, environment, file path, payload, secret, or output content is logged.
- Route-token keys rotate; nonce/replay checks apply to sensitive one-shot actions.

Measure gateway SLI counts, gateway/ingress connections and bytes, attach latency, replayed/truncated/gap bytes, stdin indeterminate writes, spool pressure, route cache lag and validation failures, readiness, drains, and egress denials. Trace gateway request to generation-specific route and first sandbox response without payload content.

## Acceptance

- Postgres/outbox failure-injection proves no `READY` route can be observed without its sandbox-state commit; delayed/duplicate outbox events cannot make a stale generation routable.
- A route selected from cache is rejected after generation, ingress epoch, worker SPIFFE ID, endpoint, or state changes. Clients cannot choose a worker destination.
- External, exec, and internal route credentials fail when used at another validator/audience; cross-project and expired credentials fail closed.
- Gateway restart during exec preserves ingress-spooled stdout/stderr and exact acknowledged replay.
- Frame conformance covers contiguous offsets, duplicate frames, ACK, EOF-before-EXIT ordering, epoch change, explicit `TRUNCATED`/`GAP`, and the full 10-minute post-exit reconnect window.
- A disconnect at every stdin write/ACK boundary proves unknown-ACK input is never auto-replayed; known unacknowledged ranges replay at most once.
- Slow readers and writers cannot exhaust memory or Local SSD; truncation is explicit. PTY detach/EOF/half-close never implicitly terminates the process and PTY stderr is correctly merged.
- Domain tests cover exact/wildcard/apex, CNAME chains, rebinding, A/AAAA parity, certificate/SNI mismatch, and blocked DoH, DoT, QUIC, ECH, proxy chaining, and direct IP. CIDR authorization remains independent.
- Metadata and management networks are unreachable under all egress modes.
- Drain commits route removal before cleanup and prevents new requests while preserving defined exec behavior.
- Load and failure tests demonstrate 99.9% gateway SLO instrumentation and p95 attach at most 1 second for ready sandboxes.
