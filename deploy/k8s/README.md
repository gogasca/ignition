# GKE manifests

Kustomize overlay for `ignition-api` and `ignition-controller`. The runbook is the [implementation guide](../../docs/guides/ignition-implementation.md#deploy-regional-dev) → `overlays/dev`.

```bash
kubectl apply -k deploy/k8s/overlays/dev
```

Create `ignition-control-plane` in `ignition-system` before the first apply (see the [implementation guide](../../docs/guides/ignition-implementation.md)). Replace project IDs and image tags in the overlay to match Artifact Registry. The overlay includes a Cloud SQL Auth Proxy sidecar; set secret key `DATABASE_URL` to `postgres://ignition:…@127.0.0.1:5432/ignition?sslmode=disable`. Optional ConfigMap `IGNITION_OIDC_JWKS_URL` if discovery from the issuer is not used. `IGNITION_ENV` is set by the overlay (`dev`).
