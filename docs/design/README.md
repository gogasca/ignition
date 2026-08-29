# Design documents

Normative architecture for Ignition. **The implementation guide and the API/controller proposal match the running binaries.** Older module designs (scheduler, fleet, hostd, checkpoint) describe the gated custom GCE runtime and are not the current deploy path. The [data plane design](ignition-design-data-plane-networking.md) likewise specifies the custom-runtime target (`ignition-ingress`, Postgres route table, exec spool); the MVP data plane is a reduced `ignition-gateway`-only path and `ignition-gateway` is not yet shipped.

Start here:

1. [GKE Sandbox MVP](ignition-design-gke-sandbox.md) — recommended implementation
2. [API and Controller proposal](ignition-design-api-controller.md) — `ignition-api` and `ignition-controller`
3. [Technical design](ignition-technical-design.md) — overview and module index
4. [Create Sandbox API](ignition-sandbox-create-api.md) — public create contract
5. [Implementation guide](../guides/ignition-implementation.md) — binaries, images, one regional GKE dev
6. [Implementation plan](gpu-sandbox-implementation-plan.md) — gated custom GCE runtime (not the deploy path)
