# Design documents

Normative architecture for Ignition. The [implementation guide](../guides/ignition-implementation.md) is the source of truth for what currently runs. The API, controller, and sandbox health/GPU-readiness path are implemented; process supervision, `ignition-gateway`, and `ignitionctl` remain incomplete. `BARE_METAL` is represented in the contract but currently fails closed.

Older module designs (scheduler, fleet, hostd, checkpoint) describe the gated custom Compute Engine runtime and are not the current deploy path. The [data plane design](ignition-design-data-plane-networking.md) likewise describes a future custom-runtime target (`ignition-ingress`, Postgres route table, exec spool), not the shipped dev slice.

Start here:

1. [GKE Sandbox MVP](ignition-design-gke-sandbox.md) — recommended implementation
2. [API and Controller proposal](ignition-design-api-controller.md) — `ignition-api` and `ignition-controller`
3. [Technical design](ignition-technical-design.md) — overview and module index
4. [Create Sandbox API](ignition-sandbox-create-api.md) — public create contract
5. [Implementation guide](../guides/ignition-implementation.md) — binaries, images, one regional GKE dev
6. [Implementation plan](gpu-sandbox-implementation-plan.md) — gated custom GCE runtime (not the deploy path)
7. [Multi-accelerator sandbox plan](multi-accelerator-sandbox-plan.md) — GPU + CPU accelerator classes (in progress)
