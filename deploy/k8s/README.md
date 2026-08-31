# GKE manifests

Kustomize manifests for `ignition-api` and `ignition-controller`. The base owns namespaces, service accounts, controller RBAC, Deployments, Services, and disruption budgets. Cloud SQL Auth Proxy components and environment overlays add configuration around that base.

The supported runbook is the [regional dev deployment](../../docs/guides/ignition-implementation.md#deploy-regional-dev) using `overlays/dev`. Do not apply that directory unchanged: it contains the placeholder project `ignition-dev`. The guide copies `deploy/k8s` to a temporary directory, replaces project-specific image, Workload Identity, sandbox repository, and Cloud SQL values there, validates it with `kubectl kustomize`, and applies the rendered copy.

Before applying an overlay, create `ignition-control-plane` in `ignition-system`. Its keys are `DATABASE_URL`, `STREAM_TOKEN_SECRET`, optional `OIDC_ISSUER`, and optional dev-only `DEV_BEARER`. The private Auth Proxy sidecar listens on loopback, so the application DSN is `postgres://ignition:…@127.0.0.1:5432/ignition?sslmode=disable`.

`overlays/staging`, `overlays/prod`, and `overlays/sample` are templates, not validated production runbooks. They include Ingress resources and fixed project/domain placeholders that must be reviewed before use. No overlay deploys `ignition-gateway` or a sandbox workload; the controller creates sandbox Pods dynamically.
