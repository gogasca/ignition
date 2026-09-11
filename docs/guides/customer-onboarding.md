# Customer onboarding

**Status:** first pass — dry-run-validated (`kubectl kustomize`, `terraform
validate` equivalents), not yet exercised end to end against a live GCP
project. Treat as a strong starting point for a pilot, not a certified
runbook.
**Audience:** whoever stands up a customer's (or your own pilot) Ignition
install — an Ignition operator, or the customer's own platform engineer.
**Prerequisites and wire-level detail:** [implementation
guide](ignition-implementation.md) — this doc assumes you've skimmed its
Prerequisites and Auth sections.

This is the path from `git clone` plus a GCP project (new or existing) to a
running Ignition control plane with one real, OIDC-authenticated project
owner — no `IGNITION_DEV_BEARER`, no hand-run SQL for the first project.

## What it is, and isn't

One command, `deploy/scripts/onboard.sh`, does the whole thing:
1. creates the GCP project if it doesn't exist yet (or reuses one you name),
   checks billing and GPU quota;
2. stores Terraform state in a per-project GCS bucket instead of your laptop;
3. `terraform apply`s the module in `deploy/terraform` (VPC, GKE, Cloud SQL,
   Artifact Registry, service accounts);
4. builds and pushes the four control-plane images plus a CPU sandbox seed
   image to that project's own registries;
5. renders `deploy/k8s/overlays/sample` (real OIDC audience, bootstrap
   project/admin, registry/SA/SQL values — all pulled from `terraform
   output`, not typed by hand) and deploys it;
6. seeds one project + owner via `IGNITION_BOOTSTRAP_PROJECT` /
   `IGNITION_BOOTSTRAP_ADMIN` (`internal/api/server.go`'s `bootstrapAdmin`,
   idempotent — a no-op once that project has an owner);
7. proves it end to end: mints a real Google ID token for the bootstrap
   admin, registers the seed image through the real image-admission API,
   and runs `ignitionctl sandbox create --wait` / `exec` / `terminate`.

It is **not** self-serve signup. There's still one operator (you, or the
customer's platform engineer) running the script with real GCP credentials.
Making project creation something a customer's own end users can do without
touching Terraform/SQL is separate, larger work — see "Adding more projects"
below.

It also deploys **one dedicated install per customer's own GCP project**
(BYOC), not a shared multi-tenant service — matching the commercial model in
[`productionization-plan-2026-09-11.md`](../design/productionization-plan-2026-09-11.md).

## Running it

```bash
export CUSTOMER_PROJECT_ID=acme-ignition       # created if it doesn't exist
export OPERATOR_CIDR="$(curl -s ifconfig.me)/32"
export BOOTSTRAP_ADMIN=platform-lead@acme.com  # must match your active gcloud account
export BILLING_ACCOUNT=012345-6789AB-CDEF01    # only needed if the project is new

deploy/scripts/onboard.sh
```

Optional: `REGION` (default `us-central1`), `INSTALL_NAME` (default =
`CUSTOMER_PROJECT_ID`, only shapes the OIDC audience string), `BOOTSTRAP_PROJECT`
(default `prj_customer`), `GPU_MAX_NODES` (default `0` — set `>0` to get an
L4 node pool; the script checks real `NVIDIA_L4_GPUS` / `GPUS_ALL_REGIONS`
quota first and refuses to `terraform apply` if there isn't enough).

Takes 15-30 minutes, most of it GKE cluster creation. Safe to re-run: it
reuses the existing project, Terraform state, and the SQL/stream-token
secrets it generated on the first run (persisted, gitignored, under
`deploy/scripts/.onboard-secrets/<project>/`) rather than regenerating them.

At the end you have:
- a GKE cluster with `ignition-api`, `ignition-controller`, `ignition-gateway`
  running, real Google OIDC auth, and a CPU sandbox pool;
- one Ignition project (`BOOTSTRAP_PROJECT`) with `BOOTSTRAP_ADMIN` as owner;
- one registered image (`img_seed`) so the customer can create a sandbox
  immediately.

## Day-to-day access

```bash
kubectl -n ignition-system port-forward svc/ignition-api 18080:8080 &
kubectl -n ignition-system port-forward svc/ignition-gateway 8443:8080 &
TOKEN="$(gcloud auth print-identity-token --audiences="https://api.${INSTALL_NAME}.ignition.dev")"
ignitionctl login --server http://127.0.0.1:18080 --token "${TOKEN}" --project "${BOOTSTRAP_PROJECT}"
```

There is no public DNS/Ingress for the API or gateway by default (matches the
working `anyscale-staging` overlay) — port-forward is the supported path
until the customer wants a real domain + managed certificate, at which point
follow `overlays/staging`'s `ingress.yaml` / `gateway-ingress.yaml` pattern.

## Adding more projects, users, or secrets

`onboard.sh` only provisions the *first* project. There is no self-serve
Project/Secret API yet — `STATUS.md` and `ROADMAP.md` both mark it
`PROPOSED`, and `internal/auth/rbac.go` has no `project.create` permission or
platform-admin concept to gate one (every existing permission check assumes
the project row already exists). Until that's built:

- **A second project**, or a second owner on the first one: run
  `db/rolebindings.sql` through the Cloud SQL Auth Proxy (the `psql`
  invocation is in that file's own header comment), or use the shipped
  `PUT /v1/projects/{project}/roleBindings/{subject}` API as an existing
  owner/admin.
- **A secret a sandbox can reference**: `db/secrets.sql` registers the
  `secretId`; the payload itself goes in Secret Manager under the project set
  by `IGNITION_GCP_PROJECT` — the row only authorizes the project to name it.

## Tearing down

```bash
cd deploy/terraform
terraform init -reconfigure \
  -backend-config="bucket=${CUSTOMER_PROJECT_ID}-ignition-tfstate" \
  -backend-config="prefix=ignition"
terraform apply -auto-approve -var-file="${CUSTOMER_PROJECT_ID}.tfvars" \
  -var "sql_password=$(cat deploy/scripts/.onboard-secrets/${CUSTOMER_PROJECT_ID}/sql_password)" \
  -var sql_deletion_protection=false
terraform destroy -var-file="${CUSTOMER_PROJECT_ID}.tfvars" \
  -var "sql_password=$(cat deploy/scripts/.onboard-secrets/${CUSTOMER_PROJECT_ID}/sql_password)"
gcloud storage rm -r "gs://${CUSTOMER_PROJECT_ID}-ignition-tfstate"
rm -rf "deploy/scripts/.onboard-secrets/${CUSTOMER_PROJECT_ID}" "deploy/terraform/${CUSTOMER_PROJECT_ID}.tfvars"
```

`sql_deletion_protection` defaults to `true` — the extra `apply` above flips
it off first, since `terraform destroy` alone refuses to drop a
deletion-protected Cloud SQL instance. Deleting the whole
`CUSTOMER_PROJECT_ID` GCP project (`gcloud projects delete`) is a faster,
coarser alternative when nothing else lives in it.

## Known gaps to fix before a real paying pilot

Carried over from
[`productionization-plan-2026-09-11.md`](../design/productionization-plan-2026-09-11.md)'s
P0 list — `onboard.sh` gets you a *running* install, not a *hardened* one:
controller lease/quota-release races, durable cleanup beyond 15 minutes,
per-project resource bounds, and pinned (non-`:mutable-tag`) production
images are all still open. Don't point a real customer's traffic at this
without reading that doc's P0 section first.
