# Design documents

Normative architecture for Ignition. The [implementation guide](../guides/ignition-implementation.md) is the source of truth for what currently runs and how it is built and deployed.

**What is built and running:** `ignition-api` and `ignition-controller` on GKE, backed by Cloud SQL, with real Google OIDC / Cloud IAP authentication and SQL-backed project RBAC. Sandbox lifecycle (create/get/list/terminate/watch), the process control plane (metadata only), and operations are implemented. CPU (`accelerator: NONE`) and `NVIDIA_L4` GPU sandbox profiles run as gVisor Pods on dedicated GKE Sandbox node pools; `ignition-gpu-agent` attests GPU identity and health. Deployed to GCP project `anyscale-demo` in the `dev` and `anyscale-staging` overlays.

**Not built:** the exec byte path (`ignition-gateway` and the `sandbox-init` process supervisor are stubs and not deployed), `ignitionctl` (every subcommand returns `not implemented`), digest-pinned images and an image-admission catalog, the Project/Image/Secret/Event public APIs (seed rows only), and writable Volume / Session snapshot resources. `BARE_METAL` is represented in the contract but fails closed.

**Deferred (custom Compute Engine runtime):** the scheduler, fleet, worker-runtime broker chain (`ignitiond`/`ignition-hostd`/`snapshotd`/`ignition-ingress`), checkpoint/restore, golden startup snapshots, and the custom lazy-image path. These module designs are retained as the design of record for a possible future optimization, gated on measured evidence that GKE cannot meet requirements; they are not the deploy path.

Start here:

1. [GKE Sandbox](ignition-design-gke-sandbox.md) — the shipped architecture
2. [API and Controller](ignition-design-api-controller.md) — `ignition-api` and `ignition-controller`
3. [Technical design](ignition-technical-design.md) — overview and module index
4. [Create Sandbox API](ignition-sandbox-create-api.md) — the public create contract
5. [Implementation guide](../guides/ignition-implementation.md) — binaries, images, and the regional GKE deploy
6. [Default runtime](ignition-design-default-runtime.md) — system-managed RuntimeSpec; optional CreateSandbox fields; CPU + L4 accelerator profiles
7. [Client API and Identity](ignition-design-client-api-identity.md) — public API surface, external identity, project RBAC
