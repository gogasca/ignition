# Ignition design documents

Normative architecture for Ignition — isolated single-GPU / CPU sandboxes for
untrusted tenant code on GKE Standard with GKE Sandbox (gVisor / `nvproxy`).

**Is X built?** → [STATUS.md](STATUS.md). One table per area.

**How do I build and deploy it?** → [implementation guide](../guides/ignition-implementation.md).
Source of truth for what actually runs.

## The documents

| Doc | Covers | Status |
|---|---|---|
| [STATUS.md](STATUS.md) | Feature-by-feature: shipped / partial / proposed / deferred | — |
| [Shipped architecture](ignition-shipped-architecture.md) | `ignition-api`, `ignition-controller`, `ignition-gateway`, `sandbox-init`, `ignition-gpu-agent`; GKE topology, the reconcile loop, the Pod profile, warm capacity, the exec data plane, GPU attestation, the default runtime, storage | **SHIPPED** |
| [API contract](ignition-api-contract.md) | Public REST surface, Google identity, project RBAC, `CreateSandbox`, state machines, idempotency, errors, `ignitionctl`, SDKs | **SHIPPED** (core); Project/Secret/Event **PROPOSED** |
| [Image delivery](ignition-image-delivery.md) | GKE image streaming today, the v0 admission slice and its security gap, and the proposed catalog / adaptive delivery / snapshot / stratification work | **PARTIAL** (v0 admission); rest **PROPOSED** |
| [Production operations](ignition-production-operations.md) | Threat model, service identity, IAM, secrets, audit, metering, SLOs, incident response, DR, release security, launch gates | **PARTIAL** — the bar, not current behavior |
| [Deferred custom runtime](ignition-deferred-runtime.md) | The custom GCE/MIG worker runtime: scheduler, fleet, worker broker chain, checkpoint/restore, milestone plan. Retained as the design of record; **not** the deploy path | **DEFERRED** |

## In one paragraph

`ignition-api` and `ignition-controller` run on a GKE CPU node pool backed by
Cloud SQL. `ignition-api` authenticates Google OIDC / Cloud IAP, authorizes
against SQL project RBAC, and admits sandboxes in one serializable transaction;
it never touches Kubernetes. `ignition-controller` — the only component with Pod
RBAC — reconciles each sandbox into a server-owned gVisor Pod on a GKE Sandbox
node: one whole GPU per `NVIDIA_L4` sandbox, or a CPU-only sandbox on a shared
pool. `ignition-gpu-agent` attests GPU identity and health before `READY`.
Inside the sandbox, `sandbox-init` supervises tenant processes and serves their
stdio; `ignition-gateway` proxies the exec WebSocket in after checking an
`ignition-api`-minted stream token. `ignitionctl` and Python/TypeScript SDKs
wrap the public API. GKE owns VM lifecycle, drivers, scheduling, and
autoscaling. The custom Compute Engine runtime in
[deferred-runtime](ignition-deferred-runtime.md) is retained as a design of
record only, gated on measured evidence that GKE cannot meet a requirement.
