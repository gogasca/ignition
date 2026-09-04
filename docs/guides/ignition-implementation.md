# Ignition implementation guide

**Status:** Matches the current binaries (`cmd/ignition-api`, `cmd/ignition-controller`)  
**Audience:** engineers building and operating the control plane  
**Architecture:** [GKE Sandbox](../design/ignition-design-gke-sandbox.md)  
**API/controller design:** [API and Controller proposal](../design/ignition-design-api-controller.md)  
**Contract:** [Create Sandbox API](../design/ignition-sandbox-create-api.md), [`api/proto/ignition/v1/`](../../api/proto/ignition/v1/)

This is the **only** build-and-deploy runbook: one regional GKE **dev** environment in one GCP project. Commands are bash and target Cloud Shell or another Linux shell. Run every block in the same shell unless the text says otherwise. Architecture stays in `docs/design/`. Overlay: `deploy/k8s/overlays/dev`.

`gcloud` here is the copy/paste bootstrap. The equivalent Terraform configuration is in `deploy/terraform`; choose one infrastructure owner per environment and do not create the same resources with both.

## What this deploys

| In scope | Not built |
|---|---|
| `SandboxService` (lifecycle + process metadata) and `OperationService` | Custom GCE MIG workers |
| Google OIDC / Cloud IAP auth, SQL project RBAC, Cloud SQL, `ignition-controller` | Digest-pinned images |
| HTTP/JSON public edge (SSE for watch); `sandbox-init` health/GPU readiness | `ignition-gateway` Dockerfile / Ingress; process exec transport |
| `secretRefs` (Secret Manager → Pod env), binary outbound internet preference | GCP network-profile provisioning for both internet modes |

Public transport is **HTTP/JSON**. Protobuf is the schema; JSON field names follow proto `json_name` (lowerCamelCase). `ignition-api` must **not** call Kubernetes. The controller is the only Pod RBAC identity. They meet in Cloud SQL.

## Prerequisites

- GCP project with billing. GPU capacity requires both regional **NVIDIA L4 quota** (`NVIDIA_L4_GPUS`) and global all-regions GPU quota (`GPUS_ALL_REGIONS`).
- Go 1.26.7+, Docker, `gcloud`, `gke-gcloud-auth-plugin`, `kubectl`, `jq`, `curl`, and `openssl`. `buf` is required only if you change protos (`buf lint` works; `buf generate` is not wired — there is no `buf.gen.yaml` yet).
- This overlay uses `IGNITION_DEV_BEARER`. Staging/prod verify Google ID tokens and (via `deploy/k8s/components/iap`) Cloud IAP assertions instead — see [Auth](#auth).
- `ignitionctl` is a stub (`not implemented`). Use `curl` against the API.

Start at the repository root and fail on unset variables or failed commands:

```bash
set -euo pipefail

export REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

for tool in docker gcloud kubectl jq curl openssl git make; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done

command -v gke-gcloud-auth-plugin >/dev/null || {
  echo "missing gke-gcloud-auth-plugin; install it before fetching cluster credentials" >&2
  echo "component install: gcloud components install gke-gcloud-auth-plugin" >&2
  echo "Debian package: sudo apt-get install google-cloud-cli-gke-gcloud-auth-plugin" >&2
  exit 1
}

if command -v go >/dev/null; then
  export GO_BIN="$(command -v go)"
elif [[ -x .tools/go/bin/go ]]; then
  export GO_BIN="${REPO_ROOT}/.tools/go/bin/go"
else
  echo "Go 1.26.7+ is required" >&2
  exit 1
fi
"${GO_BIN}" version
```

The Dockerfiles use their own pinned Go builder. `GO_BIN` is for local validation. Confirm GKE Sandbox versions and both GPU quotas before creating resources. The table formatter hides quota entries in some `gcloud` versions, so query the JSON directly:

```bash
gcloud container get-server-config --region=us-central1 --format="yaml(channels,validMasterVersions)"
gcloud compute regions describe us-central1 --format=json | jq \
  '.quotas[] | select(.metric == "NVIDIA_L4_GPUS") | {metric, limit, usage}'
gcloud compute project-info describe --format=json | jq \
  '.quotas[] | select(.metric == "GPUS_ALL_REGIONS") | {metric, limit, usage}'
```

Both available values (`limit - usage`) must be at least the `GPU_MAX` selected below. A sufficient regional L4 quota alone is not enough: GKE autoscaling fails with `FailedScaleUp` / quota exceeded when `GPUS_ALL_REGIONS` is zero. The regional deployment section performs a fail-fast check after setting the project variables.

## Generate code from protos

Protos live in `api/proto/`. Use a subshell so later deployment paths still resolve from the repository root:

```bash
command -v buf >/dev/null || { echo "buf is required to lint protos" >&2; exit 1; }
buf --version
(cd api/proto && buf lint)
```

There is no `buf.gen.yaml` yet, so `buf generate` fails. Add it when you wire codegen (`protoc-gen-go` into `api/gen/ignition/v1`). Do not hand-edit generated files.

| Proto | JSON |
|---|---|
| `project_id` | `projectId` |
| `SANDBOX_STATE_READY` | `"READY"` (strip enum prefix in the public JSON layer) |

## How the binaries work

Entrypoints: `cmd/ignition-api`, `cmd/ignition-controller`. Logic: `internal/api`, `internal/auth`, `internal/store`, `internal/k8s`.

### HTTP routes

```text
POST   /v1/projects/{project}/sandboxes
GET    /v1/projects/{project}/sandboxes
GET    /v1/projects/{project}/sandboxes/{sandbox}
POST   /v1/projects/{project}/sandboxes/{sandbox}:terminate
GET    /v1/projects/{project}/sandboxes/{sandbox}:watch

POST   /v1/projects/{project}/sandboxes/{sandbox}/processes
GET    /v1/projects/{project}/sandboxes/{sandbox}/processes
GET    /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:attach
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:signal
POST   /v1/projects/{project}/sandboxes/{sandbox}/processes/{process}:cancel

GET    /v1/projects/{project}/operations
GET    /v1/projects/{project}/operations/{operation}
GET    /v1/projects/{project}/operations/{operation}:watch
POST   /v1/projects/{project}/operations/{operation}:cancel
```

Require `Authorization: Bearer <token>` on every route except `GET /healthz` (Cloud IAP callers send `X-Goog-IAP-JWT-Assertion` instead — see below). Require `Idempotency-Key` (max 128 bytes) on create, terminate, cancel, and process create/attach/signal/cancel.

### Auth

Two identity paths reach the same `Principal{Subject, Email, Kind, Domain}` and the same RBAC:

| Path | Caller | Credential | Verification |
| --- | --- | --- | --- |
| In-cluster / impersonation | Probers, CI, service-to-service | `Authorization: Bearer <Google ID token>` | issuer `https://accounts.google.com`, `typ=JWT`, RS256, `aud` ∈ `IGNITION_OIDC_AUDIENCE` + `IGNITION_OIDC_AUDIENCES`, `email_verified`, and (users only) `hd` ∈ `IGNITION_OIDC_HOSTED_DOMAINS` |
| Through the Ingress | Human Workspace users, external automation | Cloud IAP browser flow → `X-Goog-IAP-JWT-Assertion` | issuer `https://cloud.google.com/iap`, ES256, `aud` = `IGNITION_IAP_AUDIENCE` (the backend-service resource path) |

The middleware verifies the IAP assertion header when present, otherwise the bearer. `IGNITION_OIDC_SUBJECT_CLAIM=email` makes the verified email the RBAC subject; a `*.gserviceaccount.com` email is classified as a service account (exempt from the hosted-domain check; no role restriction). First-party RFC 9068 `at+jwt` access tokens are still accepted when `IGNITION_OIDC_ALLOWED_TYPES` includes `at+jwt`.

RBAC checks the project role for `sandbox.create` / `sandbox.get` / `sandbox.terminate` / `sandbox.exec` / `process.get` / `operation.get` / `operation.cancel` / `rolebinding.get` / `rolebinding.admin`. Role lookup resolves the exact subject, then a `domain:<hd>` binding for a Workspace user.

- no/invalid credential → `401`
- no role binding, unknown IDs, **and** in-project deny of terminate/cancel → `404`
- in-project deny of create/exec (viewer) → `403`

Manage bindings over the API (owner/admin only): `GET/PUT/DELETE /v1/projects/{project}/roleBindings/{subject}` where `{subject}` is an email or `domain:<fqdn>`; the last owner cannot be removed. Bootstrap the first owner with `IGNITION_BOOTSTRAP_PROJECT` + `IGNITION_BOOTSTRAP_ADMIN` (seeds one owner when the project has none) or `db/rolebindings.sql`. The dev overlay instead sets `IGNITION_DEV_BEARER` (forbidden when `IGNITION_ENV` is staging/prod), which bypasses all of the above.

Get a token:

```bash
# Service account (has roles/iam.serviceAccountTokenCreator on the SA):
gcloud auth print-identity-token \
  --impersonate-service-account="ignition-cli@${PROJECT}.iam.gserviceaccount.com" \
  --audiences="https://api.${ENV}.ignition.dev" --include-email
# In a pod with Workload Identity: the GKE metadata server (internal/probe/gcptoken.go).
# Human, browser: open https://api.${ENV}.ignition.dev/... and complete IAP sign-in.
```

### Admission and store

Validate before the SQL transaction: `imageId` matches `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`. `resources`, `placement`, `timeouts`, `network` are optional — each unset field is merged from the system default runtime (`IGNITION_DEFAULT_RUNTIME`, built-in = CPU-only) and the resolved `RuntimeSpec` is validated: `resources.accelerator.type` in `IGNITION_ALLOWED_ACCELERATORS` (default `NONE,NVIDIA_L4`), `count` `1` for `NVIDIA_L4` / `0` for `NONE`; CPU ≤ 8000m, memory ≤ 32768 MiB; timeout caps and enum checks in `internal/store/runtime.go`; `placement.computeEnvironment` is `STANDARD` or `BARE_METAL`; `network.internetAccess` is `ENABLED` or `DISABLED`. Command/label caps stay in `internal/api/limits.go`. `GET /v1/projects/{project}/runtimes/default` returns the resolved default runtime.

In one serializable transaction: idempotency key, insert sandbox `CREATING`, insert `CREATE_SANDBOX` operation, increment `project_quota.active`. Return `202`. Same key + same hash replays; different hash → `409 IDEMPOTENCY_KEY_REUSED`. **Do not create a Kubernetes Pod here.**

Get/list are project-scoped SQL. Watch polls product state, emits content-addressed SSE snapshots on change, honors `Last-Event-ID`, sends heartbeats, and closes on terminal state or after ~60s. Terminate sets desired `TERMINATING` (`202`); permission deny is `404`. `GET /healthz` pings the store (503 if Postgres is down). The base manifests wire the API **liveness** probe to `/healthz` (1s timeout), so any Cloud SQL interruption — an in-place `gcloud sql instances patch`, a maintenance event, an HA failover, the `--availability-type` change below — makes the API pods fail liveness and restart until the instance is reachable again. This is a self-healing crashloop, not data loss; do the SQL patch in a maintenance window and expect a few `RESTARTS` on `ignition-api`.

Process APIs require `READY`. `attach` is idempotent (same key replays `streamEpoch`). Stream tokens use `IGNITION_STREAM_TOKEN_SECRET`. `signal` allowlist: `SIGTERM`, `SIGINT`, `SIGKILL`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2`. Cancel of a still-`PENDING`/`RUNNING` create fails the sandbox (`CANCELLED`) and releases quota.

Tables (`internal/store/schema.sql`, embedded by the API): `projects`, `role_bindings`, `images`, `sandboxes`, `processes`, `operations`, `idempotency_keys`, `project_quota`, `controller_leases`. This is a complete baseline schema, not a migration chain. `store.Open` (API) applies it idempotently; `store.OpenWithoutSchema` (controller) is DML only.

### Environment variables

| Variable | Who | Notes |
|---|---|---|
| `IGNITION_ENV` | both | this overlay sets `dev`; `staging` / `prod` / `production` fail closed |
| `DATABASE_URL` | both | required when `IGNITION_ENV` is staging/prod |
| `IGNITION_OIDC_ISSUER` | API | required when `IGNITION_ENV` is staging/prod; base sets `https://accounts.google.com` |
| `IGNITION_OIDC_JWKS_URL` | API | optional explicit JWKS (discovery works for Google) |
| `IGNITION_OIDC_AUDIENCE` / `IGNITION_OIDC_AUDIENCES` | API | primary + additional accepted `aud` values (CSV) |
| `IGNITION_OIDC_SUBJECT_CLAIM` | API | `email` (default for the Google issuer) or `sub` |
| `IGNITION_OIDC_HOSTED_DOMAINS` | API | CSV of Workspace domains required on user tokens; SAs exempt |
| `IGNITION_OIDC_ALLOWED_TYPES` | API | CSV of accepted JWT `typ` values; default `JWT` for Google, else `at+jwt` |
| `IGNITION_IAP_ENABLED` / `IGNITION_IAP_AUDIENCE` | API | verify `X-Goog-IAP-JWT-Assertion`; audience is the backend-service resource path (see `deploy/k8s/components/iap`) |
| `IGNITION_BOOTSTRAP_PROJECT` / `IGNITION_BOOTSTRAP_ADMIN` | API | seed one owner binding when the project has none |
| `IGNITION_STREAM_TOKEN_SECRET` | API | attach tokens; required non-default in staging/prod |
| `IGNITION_DEV_BEARER` | API | dev overlay only; forbidden in staging/prod |
| `IGNITION_GATEWAY_URL` | API | stream token audience |
| `IGNITION_ALLOWED_ACCELERATORS` | API | AcceleratorType allowlist; default `NONE,NVIDIA_L4` (alias: `IGNITION_ALLOWED_GPU_TYPES`) |
| `IGNITION_DEFAULT_RUNTIME` | API | JSON `RuntimeSpec` merged over the built-in CPU default; validated at startup |
| `IGNITION_MAX_ACTIVE_SANDBOXES` | API | per-project quota |
| `IGNITION_K8S_NAMESPACE` | controller | default `ignition-sandboxes` |
| `IGNITION_MIN_WARM` / `IGNITION_MAX_WARM` | controller | balloon Pods |
| `IGNITION_SANDBOX_IMAGE_PREFIX` | controller | Artifact Registry prefix, e.g. `us-central1-docker.pkg.dev/${PROJECT}/sandboxes` |
| `IGNITION_GCP_PROJECT` | controller | used to compose the image prefix and Secret Manager project |

Kubernetes secret keys: `DATABASE_URL`, `STREAM_TOKEN_SECRET`, optional `DEV_BEARER` (dev only). The OIDC issuer is now a ConfigMap value, not a secret. DSN through the Auth Proxy sidecar: `postgres://ignition:…@127.0.0.1:5432/ignition?sslmode=disable`.

HTTP: `ReadHeaderTimeout` 10s, `IdleTimeout` 90s (no short `WriteTimeout`, so SSE can flush). `X-Request-Id` sanitized to `[A-Za-z0-9._-]`, max 128.

### Controller

Distinct KSA. **This** binary is the only one with Pod RBAC. It must not run DDL.

Loop: list sandboxes; `BARE_METAL` currently fails with `COMPUTE_ENVIRONMENT_UNAVAILABLE` without creating a GKE Pod; an accelerator type with no `internal/k8s` profile fails `WORKLOAD_NOT_SUPPORTED` without a Pod; for `STANDARD`, skip Pod create when already `FAILED`/`FINISHED`/`TERMINATING`; resolve image as `${IGNITION_SANDBOX_IMAGE_PREFIX}/{imageId}` (or `{region}-docker.pkg.dev/${IGNITION_GCP_PROJECT}/sandboxes/{imageId}`) after the charset check (empty/invalid → `IMAGE_UNAVAILABLE`, no Pod); create Pod `sbx-{id}` from the accelerator profile — `runtimeClassName: gvisor` always; `NVIDIA_L4` gets one whole GPU, the `ignition.io/gpu-sandbox` toleration, `gpu-sandbox-l4` node pool, and hostname anti-affinity; `NONE` gets no device, the `ignition.io/sandbox` toleration, and the `cpu-sandbox` node pool — plus **read-only root filesystem**, writable `/scratch`, and `IGNITION_ACCELERATOR` env; inject `secretRefs` from Secret Manager as env (missing secret → `SECRET_UNAVAILABLE`, no Pod); map the stored internet preference to a preconfigured GCP network profile; map Pod conditions → `SCHEDULED` / `STARTED` / `READY` / `FAILED`. `sandbox-init` serves `/healthz` and `/readyz` on port 8081. For `NVIDIA_L4`, `/readyz` stats the device nodes (`/dev/nvidiactl`, `/dev/nvidia-uvm`, exactly one `/dev/nvidiaN` — presence only), runs `nvidia-smi` for a canonical `GPU-…` UUID and clean ECC, and runs the `cuda-check` helper (`cuInit()`); it never reads `NVIDIA_VISIBLE_DEVICES` or a device-node name. For `NONE`, readiness is the supervisor being up. For `NONE`, kubelet PodReady advances the state to `READY`. For `NVIDIA_L4`, the controller **also** requires `ignition-gpu-agent` to have stamped a canonical `ignition.io/gpu-uuid` (`k8s.IsCanonicalGPUUUID`) plus `ignition.io/init-healthy=true` on the Pod — the sandbox holds no Kubernetes credential and cannot self-annotate. On terminate delete the Pod. Before deleting a GPU Pod, cordon its node when the Pod carries `ignition.io/gpu-cleanup=ambiguous` **or** the node does (the latter written by `ignition-gpu-agent` after a failed reuse check): GET the node, patch only if `ignition.io/node-pool=gpu-sandbox-l4`, return the error if cordon fails. Annotate occupied GPU nodes with `cluster-autoscaler.kubernetes.io/scale-down-disabled=true`. Balloon scale-in waits 15 minutes. Never create a second Pod for the same id.

`ignition-gpu-agent` is a privileged DaemonSet on the `gpu-sandbox-l4` pool (`deploy/k8s/components/gpu-agent`, `IGNITION_NODE_NAME` from `spec.nodeName`, `nvidia-smi` from the hostPath driver mount, no `nvidia.com/gpu`). Each pass: if a sandbox Pod is on its node it inventories the single GPU (`nvidia-smi` NVML) and — when healthy, canonical-UUID, and free of residual compute processes — patches the attestation annotations; otherwise it annotates the Node `ignition.io/gpu-cleanup=ambiguous`. With no sandbox Pod it runs the same check as a reuse gate and sets/clears that Node annotation.

Controller RBAC: Pods in `ignition-sandboxes`; ClusterRole get/list/patch Nodes. No cluster-admin. API KSA has **no** Kubernetes RBAC.

### `ignitionctl`

The binary exists (`cmd/ignitionctl`) but every subcommand returns `not implemented`. Use the `curl` examples below.

---

## Deploy regional dev

One GCP project. Regional private GKE, private-IP Cloud SQL, no public Ingress. Port-forward + `IGNITION_DEV_BEARER`.

```text
Regional GKE control plane (us-central1); workers pinned to us-central1-a
  CPU pool     1–3× e2-standard-4 (total autoscaling)
  GPU pool     0–2× g2-standard-8 + 1× L4 + gVisor (private nodes)
Cloud SQL      regional-HA PostgreSQL 16, backups + PITR, private IP, Auth Proxy --private-ip
API            kubectl port-forward svc/ignition-api 8080:8080
```

`--node-locations="${ZONE}"` keeps a regional control plane without one CPU VM per zone. `--min-nodes` / `--max-nodes` are per zone; this runbook uses `--total-min-nodes` / `--total-max-nodes`.

### 1. Variables

Use a fresh, billed dev project. The commands deliberately stop on conflicting resources instead of silently reusing infrastructure with unknown settings. GCP project IDs are globally unique; replace `your-gcp-project`.

```bash
export PROJECT=your-gcp-project
export REGION=us-central1
export ZONE=us-central1-a
export CLUSTER=ignition
export NETWORK=ignition-vpc
export SUBNET=ignition-subnet
export INTERNET_SUBNET=ignition-internet-subnet
export ROUTER=ignition-router
export NAT=ignition-nat
export PSA_RANGE=ignition-psa
export MASTER_RANGE=172.16.0.0/28
export NODES_RANGE=10.10.0.0/20
export PODS_RANGE=10.20.0.0/16
export SVCS_RANGE=10.30.0.0/20
export INTERNET_NODES_RANGE=10.40.0.0/20
export INTERNET_PODS_RANGE=10.50.0.0/16
export SQL_INSTANCE=ignition-sql
export AR_REPO=ignition
export SANDBOX_REPO=sandboxes
export NODE_SA=ignition-nodes
export GPU_MAX=1
export LOCAL_API_PORT=18080
export OPERATOR_IP="$(curl -4fsS https://ifconfig.me)"
[[ "${OPERATOR_IP}" =~ ^[0-9]+(\.[0-9]+){3}$ ]] || { echo "could not determine operator IPv4 address" >&2; exit 1; }
export OPERATOR_CIDR="${OPERATOR_IP}/32"

gcloud config set project "${PROJECT}"
gcloud config set compute/region "${REGION}"
gcloud auth list --filter=status:ACTIVE --format='value(account)'
```

If the project does not exist, create and bill it first. Skip this block for an existing dedicated project.

```bash
export BILLING_ACCOUNT=XXXXXX-XXXXXX-XXXXXX
gcloud projects create "${PROJECT}" --name="Ignition dev"
gcloud billing projects link "${PROJECT}" --billing-account="${BILLING_ACCOUNT}"
gcloud config set project "${PROJECT}"
```

Verify that billing is enabled before provisioning billable resources:

```bash
[[ "$(gcloud billing projects describe "${PROJECT}" --format='value(billingEnabled)')" == "True" ]]
```

Check the available quota, not just the limit, before creating the cluster. Request regional L4 quota in the Quotas console when needed. To request global quota, enable Cloud Quotas and create the preference shown below; if the preference already exists, describe or update it instead of creating it again.

```bash
gcloud services enable compute.googleapis.com
export REGIONAL_L4_AVAILABLE="$(gcloud compute regions describe "${REGION}" --format=json | jq -er \
  '[.quotas[] | select(.metric == "NVIDIA_L4_GPUS") | (.limit - .usage)] | first | floor')"
export GLOBAL_GPU_AVAILABLE="$(gcloud compute project-info describe --project="${PROJECT}" --format=json | jq -er \
  '[.quotas[] | select(.metric == "GPUS_ALL_REGIONS") | (.limit - .usage)] | first | floor')"
printf 'regional L4 available=%s, global GPU available=%s, required=%s\n' \
  "${REGIONAL_L4_AVAILABLE}" "${GLOBAL_GPU_AVAILABLE}" "${GPU_MAX}"

if (( REGIONAL_L4_AVAILABLE < GPU_MAX || GLOBAL_GPU_AVAILABLE < GPU_MAX )); then
  echo "GPU quota is insufficient; obtain approval before continuing" >&2
  exit 1
fi
```

Global quota request, only when the preceding check reports that it is needed:

```bash
export QUOTA_CONTACT_EMAIL="$(gcloud config get-value account)"
gcloud services enable cloudquotas.googleapis.com
gcloud quotas preferences create \
  --project="${PROJECT}" --billing-project="${PROJECT}" \
  --service=compute.googleapis.com \
  --quota-id=GPUS-ALL-REGIONS-per-project \
  --preferred-value="${GPU_MAX}" \
  --email="${QUOTA_CONTACT_EMAIL}" \
  --justification="Ignition regional development GKE L4 sandbox pool" \
  --preference-id=ignition-global-gpu
```

Inspect an existing request with `gcloud quotas preferences describe ignition-global-gpu --project="${PROJECT}" --service=compute.googleapis.com`. Wait for approval and re-run the fail-fast check before provisioning the GPU pool.

### 2. APIs, VPC, GKE, GPU pool, SQL, AR, IAM

Private nodes, no public IPs on sandbox VMs. Restricted and internet-enabled sandbox pools run in the same GKE cluster but on separate node pools. The internet-enabled pools use a separate subnet, Pod range, node network tag, and Cloud NAT scope. Do **not** enable GPU time-sharing, MPS, or MIG. Both GKE and the primary Cloud SQL instance are regional; the optional SQL DR replica is cross-region.

The cluster is created with **GKE Dataplane V2** (`--enable-dataplane-v2`) for cluster-owned defense in depth. Client networking input is not converted into Kubernetes NetworkPolicy. `network.internetAccess` selects a preconfigured GCP network profile; VPC, subnet, firewall, NAT, and metadata protections are the enforcement boundary.

```bash
gcloud services enable \
  container.googleapis.com compute.googleapis.com sqladmin.googleapis.com \
  artifactregistry.googleapis.com secretmanager.googleapis.com \
  iamcredentials.googleapis.com cloudresourcemanager.googleapis.com \
  servicenetworking.googleapis.com cloudbuild.googleapis.com \
  containerfilesystem.googleapis.com dns.googleapis.com

gcloud compute networks create "${NETWORK}" --subnet-mode=custom
gcloud compute networks subnets create "${SUBNET}" \
  --network="${NETWORK}" --region="${REGION}" --range="${NODES_RANGE}" \
  --secondary-range=pods="${PODS_RANGE}",svcs="${SVCS_RANGE}" \
  --enable-private-ip-google-access
gcloud compute networks subnets create "${INTERNET_SUBNET}" \
  --network="${NETWORK}" --region="${REGION}" --range="${INTERNET_NODES_RANGE}" \
  --secondary-range=internet-pods="${INTERNET_PODS_RANGE}" \
  --enable-private-ip-google-access

gcloud compute routers create "${ROUTER}" --network="${NETWORK}" --region="${REGION}"
gcloud compute routers nats create "${NAT}" \
  --router="${ROUTER}" --region="${REGION}" \
  --nat-custom-subnet-ip-ranges="${INTERNET_SUBNET}:ALL" \
  --auto-allocate-nat-external-ips

gcloud compute addresses create "${PSA_RANGE}" --global \
  --purpose=VPC_PEERING --prefix-length=16 --network="${NETWORK}"
gcloud services vpc-peerings connect \
  --service=servicenetworking.googleapis.com \
  --ranges="${PSA_RANGE}" --network="${NETWORK}"

# --- Node egress lockdown ----------------------------------------------------
# A node (and any Pod egressing through it) may reach ONLY this VPC, the GKE
# control plane, Google APIs, and the private Cloud SQL range. Every other
# destination -- other VPCs, the public internet -- is dropped.
#
# Pin Google APIs and image pulls to the Private Google Access VIP so they never
# need a public route: point googleapis.com / *.pkg.dev / *.gcr.io at it.
export PGA_VIP=199.36.153.8/30
gcloud compute routes create ignition-private-google-apis \
  --network="${NETWORK}" --destination-range="${PGA_VIP}" \
  --next-hop-gateway=default-internet-gateway --priority=1000

for zone_domain in googleapis.com pkg.dev gcr.io; do
  zone_name="ignition-${zone_domain//./-}"
  gcloud dns managed-zones create "${zone_name}" \
    --dns-name="${zone_domain}." --visibility=private --networks="${NETWORK}" \
    --description="Ignition: pin ${zone_domain} to the Private Google Access VIP"
  gcloud dns record-sets create "${zone_domain}." --zone="${zone_name}" \
    --type=A --ttl=300 --rrdatas=199.36.153.8,199.36.153.9,199.36.153.10,199.36.153.11
  gcloud dns record-sets create "*.${zone_domain}." --zone="${zone_name}" \
    --type=CNAME --ttl=300 --rrdatas="${zone_domain}."
done

# CIDR that Service Networking allocated for private Cloud SQL.
export PSA_CIDR="$(gcloud compute addresses describe "${PSA_RANGE}" --global \
  --format='csv[no-heading](address,prefixLength)' | tr ',' '/')"

# Deny-all egress for tagged nodes, then higher-priority (lower number) allows.
gcloud compute firewall-rules create ignition-node-egress-deny \
  --network="${NETWORK}" --direction=EGRESS --action=DENY --rules=all \
  --destination-ranges=0.0.0.0/0 --priority=65500 --target-tags=ignition-node

gcloud compute firewall-rules create ignition-node-egress-allow-cluster \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW --rules=all \
  --destination-ranges="${NODES_RANGE},${PODS_RANGE},${SVCS_RANGE},${INTERNET_NODES_RANGE},${INTERNET_PODS_RANGE}" \
  --priority=1000 --target-tags=ignition-node

gcloud compute firewall-rules create ignition-node-egress-allow-control-plane \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW \
  --rules=tcp:443,tcp:8132,tcp:10250 --destination-ranges="${MASTER_RANGE}" \
  --priority=1000 --target-tags=ignition-node

gcloud compute firewall-rules create ignition-node-egress-allow-google-apis \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW --rules=tcp:443 \
  --destination-ranges="${PGA_VIP}" --priority=1000 --target-tags=ignition-node

gcloud compute firewall-rules create ignition-node-egress-allow-cloudsql \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW --rules=tcp:3307 \
  --destination-ranges="${PSA_CIDR}" --priority=1000 --target-tags=ignition-node

# Internet-enabled sandbox nodes use a separate tag and subnet. They may reach
# public destinations through Cloud NAT, but private/link-local ranges are still
# denied after the explicit cluster/control-plane/PGA allows.
gcloud compute firewall-rules create ignition-internet-node-egress-allow-cluster \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW --rules=all \
  --destination-ranges="${NODES_RANGE},${PODS_RANGE},${SVCS_RANGE},${INTERNET_NODES_RANGE},${INTERNET_PODS_RANGE}" \
  --priority=800 --target-tags=ignition-sandbox-internet

gcloud compute firewall-rules create ignition-internet-node-egress-allow-control-plane \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW --rules=all \
  --destination-ranges="${MASTER_RANGE},${PGA_VIP}" \
  --priority=800 --target-tags=ignition-sandbox-internet

gcloud compute firewall-rules create ignition-internet-node-egress-deny-private \
  --network="${NETWORK}" --direction=EGRESS --action=DENY --rules=all \
  --destination-ranges=10.0.0.0/8,100.64.0.0/10,127.0.0.0/8,169.254.0.0/16,172.16.0.0/12,192.168.0.0/16 \
  --priority=900 --target-tags=ignition-sandbox-internet

gcloud compute firewall-rules create ignition-internet-node-egress-allow-public \
  --network="${NETWORK}" --direction=EGRESS --action=ALLOW --rules=all \
  --destination-ranges=0.0.0.0/0 \
  --priority=1000 --target-tags=ignition-sandbox-internet
# ---------------------------------------------------------------------------

gcloud iam service-accounts create "${NODE_SA}" \
  --display-name="Ignition GKE nodes"
gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/container.defaultNodeServiceAccount

gcloud container clusters create "${CLUSTER}" \
  --region="${REGION}" --release-channel=regular \
  --network="${NETWORK}" --subnetwork="${SUBNET}" \
  --cluster-secondary-range-name=pods --services-secondary-range-name=svcs \
  --enable-ip-alias --enable-private-nodes \
  --enable-dataplane-v2 \
  --enable-master-authorized-networks \
  --master-authorized-networks="${OPERATOR_CIDR}" \
  --no-enable-private-endpoint --master-ipv4-cidr="${MASTER_RANGE}" \
  --workload-pool="${PROJECT}.svc.id.goog" --enable-image-streaming \
  --image-type=COS_CONTAINERD \
  --tags=ignition-node \
  --node-locations="${ZONE}" \
  --service-account="${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --scopes=cloud-platform \
  --machine-type=e2-standard-4 --num-nodes=1 \
  --enable-autoscaling --total-min-nodes=1 --total-max-nodes=3 \
  --disk-type=pd-balanced --disk-size=50 \
  --enable-autorepair --enable-autoupgrade \
  --addons=HttpLoadBalancing,GcePersistentDiskCsiDriver,HorizontalPodAutoscaling \
  --logging=SYSTEM --monitoring=SYSTEM

gcloud container clusters get-credentials "${CLUSTER}" --region="${REGION}"

gcloud container clusters update "${CLUSTER}" --region="${REGION}" \
  --additional-ip-ranges="subnetwork=${INTERNET_SUBNET},pod-ipv4-range=internet-pods"

gcloud container node-pools create gpu-sandbox-l4 \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --machine-type=g2-standard-8 \
  --service-account="${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --scopes=cloud-platform \
  --accelerator=type=nvidia-l4,count=1,gpu-driver-version=default \
  --image-type=COS_CONTAINERD --sandbox=type=gvisor \
  --num-nodes=0 --enable-autoscaling --total-min-nodes=0 --total-max-nodes="${GPU_MAX}" \
  --disk-type=pd-balanced --disk-size=100 \
  --node-labels=ignition.io/node-pool=gpu-sandbox-l4 \
  --node-taints=ignition.io/gpu-sandbox=true:NoSchedule \
  --tags=ignition-node \
  --enable-private-nodes --enable-autorepair --enable-autoupgrade

# CPU sandbox pool: the default runtime is CPU-only, so bare CreateSandbox
# requests land here. gVisor, no accelerator, scale-to-zero.
# CPU_SANDBOX_MACHINE can be any gVisor-capable type; the seed sandbox only asks
# for 1 vCPU / 2 GiB. e2-standard-8 has the broadest regional availability.
export CPU_SANDBOX_MACHINE="${CPU_SANDBOX_MACHINE:-e2-standard-8}"
gcloud container node-pools create cpu-sandbox \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --machine-type="${CPU_SANDBOX_MACHINE}" \
  --service-account="${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --scopes=cloud-platform \
  --image-type=COS_CONTAINERD --sandbox=type=gvisor \
  --num-nodes=0 --enable-autoscaling --total-min-nodes=0 --total-max-nodes=3 \
  --disk-type=pd-balanced --disk-size=100 \
  --node-labels=ignition.io/node-pool=cpu-sandbox \
  --node-taints=ignition.io/sandbox=true:NoSchedule \
  --tags=ignition-node \
  --enable-private-nodes --enable-autorepair --enable-autoupgrade

gcloud container node-pools create cpu-sandbox-internet \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --subnetwork="${INTERNET_SUBNET}" \
  --pod-ipv4-range=internet-pods \
  --machine-type="${CPU_SANDBOX_MACHINE}" \
  --service-account="${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --scopes=cloud-platform \
  --image-type=COS_CONTAINERD --sandbox=type=gvisor \
  --num-nodes=0 --enable-autoscaling --total-min-nodes=0 --total-max-nodes=3 \
  --disk-type=pd-balanced --disk-size=100 \
  --node-labels=ignition.io/node-pool=cpu-sandbox-internet \
  --node-taints=ignition.io/sandbox=true:NoSchedule \
  --tags=ignition-sandbox-internet \
  --enable-private-nodes --enable-autorepair --enable-autoupgrade

gcloud sql instances create "${SQL_INSTANCE}" \
  --database-version=POSTGRES_16 --edition=ENTERPRISE \
  --tier=db-custom-1-3840 \
  --region="${REGION}" --availability-type=REGIONAL \
  --network="${NETWORK}" --no-assign-ip \
  --storage-auto-increase \
  --backup-start-time=03:00 --retained-backups-count=14 \
  --enable-point-in-time-recovery --retained-transaction-log-days=7 \
  --deletion-protection --retain-backups-on-delete
gcloud sql databases create ignition --instance="${SQL_INSTANCE}"

gcloud artifacts repositories create "${AR_REPO}" \
  --repository-format=docker --location="${REGION}" \
  --description="Ignition control-plane images"
gcloud artifacts repositories create "${SANDBOX_REPO}" \
  --repository-format=docker --location="${REGION}" \
  --description="Ignition sandbox images"
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

for SA in ignition-api ignition-controller; do
  gcloud iam service-accounts create "${SA}" --display-name="Ignition ${SA}"
  gcloud iam service-accounts add-iam-policy-binding \
    "${SA}@${PROJECT}.iam.gserviceaccount.com" \
    --role=roles/iam.workloadIdentityUser \
    --member="serviceAccount:${PROJECT}.svc.id.goog[ignition-system/${SA}]"
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${SA}@${PROJECT}.iam.gserviceaccount.com" \
    --role=roles/cloudsql.client
done

gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:ignition-controller@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/secretmanager.secretAccessor

gcloud artifacts repositories add-iam-policy-binding "${AR_REPO}" \
  --location="${REGION}" \
  --member="serviceAccount:${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/artifactregistry.reader
gcloud artifacts repositories add-iam-policy-binding "${SANDBOX_REPO}" \
  --location="${REGION}" \
  --member="serviceAccount:${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/artifactregistry.reader
```

The SQL settings above address automated-backup, PITR, zonal-failover, and accidental-deletion findings. `REGIONAL` is synchronous high availability across two zones in one region; it is not protection from a whole-region outage. It also costs more than the former zonal dev configuration. For an existing instance created by an older version of this guide, update it in place (the availability change restarts the instance, so schedule downtime):

```bash
gcloud sql instances patch "${SQL_INSTANCE}" \
  --availability-type=REGIONAL \
  --backup-start-time=03:00 --retained-backups-count=14 \
  --enable-point-in-time-recovery --retained-transaction-log-days=7 \
  --deletion-protection --retain-backups-on-delete

gcloud sql instances describe "${SQL_INSTANCE}" \
  --format='yaml(settings.availabilityType,settings.backupConfiguration,settings.deletionProtectionEnabled,settings.retainBackupsOnDelete)'
```

Verify that the output shows `REGIONAL`, backups and PITR enabled, and deletion protection enabled. Security findings can remain visible until the service reevaluates the resource.

To address the separate multi-region disaster-recovery finding, create a cross-region replica. This adds another billable database; pick a region that meets the workload's residency and latency requirements, monitor replication lag, and test the promotion/runbook before treating it as DR:

```bash
export DR_REGION=us-east1
export SQL_DR_INSTANCE="${SQL_INSTANCE}-dr"
gcloud sql instances create "${SQL_DR_INSTANCE}" \
  --master-instance-name="${SQL_INSTANCE}" \
  --region="${DR_REGION}" --availability-type=REGIONAL \
  --tier=db-custom-1-3840 \
  --network="${NETWORK}" --no-assign-ip \
  --deletion-protection

gcloud sql instances describe "${SQL_DR_INSTANCE}" \
  --format='yaml(name,region,state,masterInstanceName,settings.availabilityType,replicaConfiguration)'
```

Cross-region PostgreSQL replication is asynchronous, so promotion can lose transactions that have not reached the replica. Keep the primary's regional HA and backups even when the DR replica exists. If this is intentionally a low-cost disposable dev environment, omit the replica and document acceptance of the multi-region finding rather than claiming regional HA covers it.

### Node egress is default-deny

The `ignition-node` tag on restricted pools plus the `ignition-node-egress-*` rules close the node to everything except the cluster ranges, the control-plane CIDR (`tcp:443,8132,10250`), the Private Google Access VIP (`199.36.153.8/30:443`), and the private Cloud SQL range (`tcp:3307`). The priority-65500 deny overrides the implied allow-egress; there is no route to any other VPC or to the public internet. This is what enforces the internet-**disabled** sandbox default at the infrastructure layer. DHCP, NTP, and the metadata server (`169.254.169.254`) are always permitted by GCP regardless of these rules, so node bootstrap and Workload Identity are unaffected; sandbox metadata isolation stays with the GKE Metadata Server as before.

The `ignition-sandbox-internet` tag on internet-enabled sandbox pools uses the
same GKE cluster but a separate subnet and firewall profile. It explicitly
allows cluster/control-plane/Private Google Access traffic, denies private and
link-local ranges, and then allows public `0.0.0.0/0` through Cloud NAT. Cloud
NAT is scoped to `${INTERNET_SUBNET}:ALL`, not every subnet in the VPC.

Consequences:

- **Every image the cluster pulls must resolve to `*.pkg.dev` or `*.gcr.io`.** Control-plane images are in Artifact Registry; runtime bases are `gcr.io/distroless/*`; the GPU seed base is the in-region `nvidia/cuda` mirror. Mirror any other third-party image (notably `postgres:16`, used by the bootstrap Pod in step 4) into `${AR_REPO}` first — a Docker Hub pull will hang and time out.
- **Cloud NAT is used only by internet-enabled sandbox nodes.** Restricted nodes keep the default-deny `ignition-node` tag and do not depend on NAT for image pulls or Google APIs.
- **Build machines are unaffected.** `make push-images`, `docker build/push`, and `buf` run on your workstation or Cloud Build, not on cluster nodes.

The intended IAM boundary is:

| Identity | Required access | Explicitly not granted |
|---|---|---|
| `ignition-api` GSA | `roles/cloudsql.client`; impersonation only from `ignition-system/ignition-api` | Kubernetes RBAC, Secret Manager, Artifact Registry |
| `ignition-controller` GSA | `roles/cloudsql.client`, `roles/secretmanager.secretAccessor`; impersonation only from `ignition-system/ignition-controller` | Artifact Registry administration, broad Kubernetes IAM |
| `ignition-nodes` GSA | `roles/container.defaultNodeServiceAccount`; repository-level Artifact Registry reader | Editor, Cloud SQL, Secret Manager |
| `ignition-controller` KSA | Namespaced sandbox Pod RBAC; node `get`, `list`, `patch` | Secrets, workloads in other namespaces, cluster-admin |
| API, gateway, sandbox KSAs | No Kubernetes RBAC | Controller permissions and Google Cloud workload identity for gateway/sandboxes |

Project-level Secret Manager access is currently necessary because the API accepts project-local `secretRefs` dynamically. Use a dedicated Ignition GCP project that contains no unrelated secrets. If deployments use a fixed secret inventory, replace the project binding with `roles/secretmanager.secretAccessor` bindings on only those Secret Manager resources.

Do not grant Artifact Registry reader to the controller GSA: kubelet pulls images using the node GSA. Do not rely on the default Compute Engine service account; older projects commonly leave it with the broad Editor role.

Confirm the cluster, node-pool configuration, and RuntimeClass. If any command fails, stop:

```bash
# gpu-sandbox-l4 exists only when GPU quota was available; skip this line for a CPU-only bring-up.
gcloud container node-pools describe gpu-sandbox-l4 \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --format='yaml(config.accelerators,config.sandboxConfig,config.serviceAccount,autoscaling)'
gcloud container node-pools describe cpu-sandbox \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --format='yaml(config.machineType,config.sandboxConfig,config.labels,config.taints,autoscaling)'
gcloud container node-pools describe cpu-sandbox-internet \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --format='yaml(config.machineType,config.sandboxConfig,config.labels,config.taints,networkConfig,autoscaling)'
gcloud container clusters describe "${CLUSTER}" --region="${REGION}" \
  --format='yaml(network,subnetwork,networkConfig.datapathProvider,ipAllocationPolicy.additionalIpRangesConfigs,nodePools[].config.serviceAccount)'
kubectl get runtimeclass gvisor
kubectl -n kube-system get ds anetd
gcloud compute firewall-rules list \
  --filter="network=${NETWORK} AND (name~^ignition-node-egress OR name~^ignition-internet-node-egress)" \
  --format='table(name,priority,direction,allowed[].map().firewall_rule().list(),denied[].map().firewall_rule().list())'
gcloud compute routers nats describe "${NAT}" --router="${ROUTER}" --region="${REGION}" \
  --format='yaml(sourceSubnetworkIpRangesToNat,subnetworks)'
```

`datapathProvider` should read `ADVANCED_DATAPATH` (Dataplane V2). The firewall list must show the restricted `ignition-node` deny-all plus its higher-priority allows, and the internet-profile rules for `ignition-sandbox-internet`: cluster/control-plane/PGA allows, private-range deny, and public allow. NAT must be `LIST_OF_SUBNETWORKS` and include only `${INTERNET_SUBNET}:ALL`. Internet-access enforcement must not depend on client-authored Kubernetes policy.

### 3. Namespaces, identities, and PriorityClasses

Apply the repository-owned KSAs before the temporary database bootstrap Pod. The annotations match the overlay and let the Auth Proxy use Workload Identity Federation.

```bash
kubectl apply -f deploy/k8s/base/namespaces.yaml
kubectl apply -f deploy/k8s/base/serviceaccounts.yaml
kubectl -n ignition-system annotate serviceaccount ignition-api \
  "iam.gke.io/gcp-service-account=ignition-api@${PROJECT}.iam.gserviceaccount.com" --overwrite
kubectl -n ignition-system annotate serviceaccount ignition-controller \
  "iam.gke.io/gcp-service-account=ignition-controller@${PROJECT}.iam.gserviceaccount.com" --overwrite

kubectl apply -f - <<'EOF'
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: { name: ignition-sandbox }
value: 1000
globalDefault: false
---
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: { name: ignition-balloon }
value: -10
globalDefault: false
EOF
```

The API defaults `network.internetAccess` to `DISABLED`, and step 2's node-egress lockdown is the current enforcement for that default. Internet-enabled requests must schedule only onto the matching `*-internet` node pool. The current application no longer creates per-sandbox NetworkPolicy resources and its KSA has no NetworkPolicy RBAC. Before advertising either mode, verify that `DISABLED` has no public egress while both modes block metadata, private control ranges, cross-tenant traffic, and unsolicited ingress. Never silently place a `DISABLED` request onto an internet-enabled profile.


### 4. Bootstrap Cloud SQL

The applications use the password user `ignition`; the Auth Proxy uses the KSA/GSA identity only to open the Cloud SQL connection. The API applies schema on startup, so `ignition` must own the database. Use distinct random passwords for the application and the `postgres` bootstrap user.

Node egress is Google-only (step 2), so the bootstrap Pod cannot pull `postgres:16` from Docker Hub. Mirror it into Artifact Registry once:

```bash
docker pull postgres:16
docker tag postgres:16 "${REGION}-docker.pkg.dev/${PROJECT}/${AR_REPO}/postgres:16"
docker push "${REGION}-docker.pkg.dev/${PROJECT}/${AR_REPO}/postgres:16"
```

```bash
export POSTGRES_PASS="$(openssl rand -hex 24)"
export SQL_PASS="${SQL_PASS:-$(openssl rand -hex 24)}"

cleanup_db_bootstrap() {
  kubectl -n ignition-system delete pod ignition-db-bootstrap --ignore-not-found --wait=true
  kubectl -n ignition-system delete secret ignition-db-bootstrap --ignore-not-found
}
trap cleanup_db_bootstrap EXIT

gcloud sql users set-password postgres \
  --instance="${SQL_INSTANCE}" --password="${POSTGRES_PASS}"
if [[ "${SQL_USER_ALREADY_EXISTS:-false}" != "true" ]]; then
  gcloud sql users create ignition \
    --instance="${SQL_INSTANCE}" --password="${SQL_PASS}"
fi

kubectl -n ignition-system create secret generic ignition-db-bootstrap \
  --from-literal=POSTGRES_PASSWORD="${POSTGRES_PASS}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ignition-db-bootstrap
  namespace: ignition-system
spec:
  serviceAccountName: ignition-api
  automountServiceAccountToken: true
  restartPolicy: Never
  containers:
    - name: psql
      image: ${REGION}-docker.pkg.dev/${PROJECT}/${AR_REPO}/postgres:16
      command: ["sh", "-c", "trap : TERM INT; sleep infinity & wait"]
      env:
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: ignition-db-bootstrap
              key: POSTGRES_PASSWORD
    - name: cloud-sql-proxy
      image: gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.15.2
      args:
        - "--structured-logs"
        - "--private-ip"
        - "--port=5432"
        - "${PROJECT}:${REGION}:${SQL_INSTANCE}"
EOF

kubectl -n ignition-system wait \
  --for=condition=Ready pod/ignition-db-bootstrap --timeout=180s
kubectl -n ignition-system exec -i ignition-db-bootstrap -c psql -- sh -ceu '
  for attempt in $(seq 1 30); do
    pg_isready -h 127.0.0.1 -p 5432 -U postgres -d ignition && break
    sleep 2
  done
  pg_isready -h 127.0.0.1 -p 5432 -U postgres -d ignition
  PGPASSWORD="${POSTGRES_PASSWORD}" psql \
    "host=127.0.0.1 port=5432 user=postgres dbname=ignition sslmode=disable" \
    -v ON_ERROR_STOP=1
' < db/grants.sql

cleanup_db_bootstrap
trap - EXIT
unset POSTGRES_PASS
```

The temporary Pod runs inside the VPC because a private-IP Cloud SQL instance is not reachable from ordinary Cloud Shell or a laptop without a private network path. `db/grants.sql` temporarily grants the `ignition` role to the Cloud SQL bootstrap user before transferring database/schema ownership, then revokes it. This is required because Cloud SQL's `postgres` user is not a PostgreSQL superuser. The cleanup trap prevents bootstrap credentials and the temporary Pod from being left behind when `psql` fails.

When Terraform owns the instance, it already creates the `ignition` user. Before running this section, set `SQL_PASS` to the exact value supplied as `TF_VAR_sql_password` and set `SQL_USER_ALREADY_EXISTS=true`; the conditional then preserves Terraform ownership instead of trying to create the user twice.

### 5. Images, rendered overlay, and control-plane secret

Dockerfiles: `deploy/docker/ignition-api.Dockerfile`, `ignition-controller.Dockerfile`. Do not put the sandbox GPU image in the same Dockerfile.

`make push-images` also builds `ignition-gateway`, `ignition-prober`, and `ignition-gpu-agent` (the last needs CGO for `cuda-check`). The dev overlay only deploys `ignition-api` and `ignition-controller`; for a CPU-only bring-up build just those two:

```bash
export AR="${REGION}-docker.pkg.dev/${PROJECT}/${AR_REPO}"
for c in ignition-api ignition-controller; do
  docker build -f "deploy/docker/${c}.Dockerfile" -t "${AR}/${c}:dev" .
  docker push "${AR}/${c}:dev"
done
# or the full set: make push-images IMAGE_REGISTRY="${AR}" IMAGE_TAG=dev

# Sandbox seed image. The dev bearer seeds exactly one image id -- `img_seed` --
# and the controller resolves it to ${SANDBOX_REPO}/img_seed (implicit :latest).
# Push the variant that matches the pool you will exercise; only one can be the
# active `img_seed` at a time.

# CPU variant (accelerator: NONE) — minimal, no CUDA. Use this for the CPU quick check.
export SANDBOX_IMAGE="${REGION}-docker.pkg.dev/${PROJECT}/${SANDBOX_REPO}/img_seed:latest"
docker build -f images/sandbox-init/Dockerfile -t "${SANDBOX_IMAGE}" .
docker push "${SANDBOX_IMAGE}"

# GPU variant (accelerator: NVIDIA_L4) — FROM a same-region mirror of
# nvidia/cuda:*-base so the injected nvidia-smi runs and cuda-check can dlopen
# libcuda.so.1. Keep CUDA_BASE at/under the GKE L4 default driver's CUDA version.
# Tag it as the same `img_seed` id (overwrites the CPU variant above).
#   export CUDA_BASE="${REGION}-docker.pkg.dev/${PROJECT}/mirror/nvidia/cuda:12.4.1-base-ubuntu22.04"
#   docker build -f images/sandbox-init/gpu.Dockerfile --build-arg CUDA_BASE="${CUDA_BASE}" -t "${SANDBOX_IMAGE}" .
#   docker push "${SANDBOX_IMAGE}"

export DEPLOY_RENDER_DIR="$(mktemp -d)"
cp -R deploy/k8s "${DEPLOY_RENDER_DIR}/k8s"
for file in \
  "${DEPLOY_RENDER_DIR}/k8s/overlays/dev/kustomization.yaml" \
  "${DEPLOY_RENDER_DIR}/k8s/overlays/dev/config.yaml" \
  "${DEPLOY_RENDER_DIR}/k8s/overlays/dev/serviceaccount-wi.yaml" \
  "${DEPLOY_RENDER_DIR}/k8s/overlays/dev/cloud-sql-instance.yaml"; do
  sed -i "s#ignition-dev#${PROJECT}#g" "${file}"
done

if grep -R "ignition-dev" "${DEPLOY_RENDER_DIR}/k8s/overlays/dev"; then
  echo "unrendered ignition-dev placeholder remains" >&2
  exit 1
fi
kubectl kustomize "${DEPLOY_RENDER_DIR}/k8s/overlays/dev" >/dev/null
```

This leaves the tracked dev overlay unchanged. As an alternative to `make push-images`, Cloud Build can test and build the control-plane images; a manual submission must supply `SHORT_SHA` because that substitution is populated automatically only for triggered builds:

```bash
export SHORT_SHA="$(git rev-parse --short=7 HEAD)"
gcloud builds submit . --config=deploy/cloudbuild/build.yaml \
  --substitutions="_EXTRA_TAG=dev,_REGION=${REGION},_AR_REPO=${AR_REPO},SHORT_SHA=${SHORT_SHA}"
```

### Test layers and PostgreSQL

Use package tests for domain behavior, `internal/store` for SQL correctness, and
`tests/integration` for API/controller workflows. The SQL tests are not mocks: the package starts
`postgres:16-alpine` through Testcontainers, applies the complete embedded schema, exercises pgx
queries and transactions, and removes the container after the package finishes.

```bash
# Everything. Requires Docker because internal/store starts PostgreSQL.
go test ./... -count=1

# Schema, constraints, queries, transactions, idempotency, quota, and leases.
go test ./internal/store -count=1

# Cross-package workflows using the Memory store and k8s.Fake.
go test ./tests/integration/... -count=1
```

If CI already provisions a disposable PostgreSQL 16 database, set
`IGNITION_TEST_DATABASE_URL=postgres://...` and the store tests use it instead of starting a
container. Never point this variable at staging or production. A missing Docker daemon, image-pull
failure, schema error, or SQL failure fails the suite; PostgreSQL coverage is not silently skipped.

`deploy/cloudbuild/build.yaml` runs `go test ./...` and also supports its explicitly provisioned
PostgreSQL test step through `IGNITION_TEST_DATABASE_URL`. The automated PR-merge / nightly /
noon-staging pipeline built on it is documented in [`deploy/PIPELINE.md`](../../deploy/PIPELINE.md).

Create or update the control-plane secret declaratively, deploy the rendered overlay, and require both rollouts to complete:

```bash
kubectl -n ignition-system create secret generic ignition-control-plane \
  --from-literal=STREAM_TOKEN_SECRET="$(openssl rand -base64 48)" \
  --from-literal=DEV_BEARER="dev-only-token" \
  --from-literal=DATABASE_URL="postgres://ignition:${SQL_PASS}@127.0.0.1:5432/ignition?sslmode=disable" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -k "${DEPLOY_RENDER_DIR}/k8s/overlays/dev"
kubectl -n ignition-system rollout status deploy/ignition-api --timeout=5m
kubectl -n ignition-system rollout status deploy/ignition-controller --timeout=5m
kubectl -n ignition-system get pods
rm -rf -- "${DEPLOY_RENDER_DIR}"
unset DEPLOY_RENDER_DIR
```

`IGNITION_ENV=dev` allows `DEV_BEARER`. The overlay sets `IGNITION_MIN_WARM=0` so the controller does **not** create balloon Pods (that would scale the L4 pool immediately). There is no Ingress; do not reserve a global IP. Workload Identity emails in `serviceaccount-wi.yaml` must match `${SA}@${PROJECT}.iam.gserviceaccount.com`. The Auth Proxy sets `automountServiceAccountToken: true` on the API pod so WI can mint tokens for the sidecar — still no Pod RBAC.

### 6. Hit the API

Run the port-forward in the background so the checks remain in the same shell. `DEV_BEARER` maps to subject `dev`, which is seeded as owner on `prj_dev` with image `img_seed`.

```bash
export PORT_FORWARD_LOG="$(mktemp)"
kubectl -n ignition-system port-forward --address=127.0.0.1 \
  svc/ignition-api "${LOCAL_API_PORT}:8080" \
  >"${PORT_FORWARD_LOG}" 2>&1 &
export PORT_FORWARD_PID=$!
trap 'kill "${PORT_FORWARD_PID}" 2>/dev/null || true' EXIT

for attempt in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${LOCAL_API_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail-with-body -sS "http://127.0.0.1:${LOCAL_API_PORT}/healthz"
curl --fail-with-body -sS \
  -H "Authorization: Bearer dev-only-token" \
  "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes" | jq .
```

Port `18080` avoids the common local Jupyter conflict on `8080`. Keeping `--address=127.0.0.1` also prevents the development bearer endpoint from listening on external interfaces.

Then [verify the implemented sandbox path](#verify-the-implemented-sandbox-path). Creating a sandbox scales the L4 pool up to `GPU_MAX`.

### Testing real authentication

The dev overlay above uses `IGNITION_DEV_BEARER`. To exercise the Google ID token path:

**Local (no cluster).** In-memory store, bootstrap yourself as owner, accept the `gcloud` user client id as an audience:

```bash
go build -o /tmp/ignition-api ./cmd/ignition-api
IGNITION_ENV=local IGNITION_LISTEN_ADDR=127.0.0.1:18080 IGNITION_ADMIN_ADDR=127.0.0.1:19090 \
IGNITION_OIDC_ISSUER=https://accounts.google.com \
IGNITION_OIDC_AUDIENCES=32555940559.apps.googleusercontent.com \
IGNITION_BOOTSTRAP_PROJECT=prj_local \
IGNITION_BOOTSTRAP_ADMIN="$(gcloud config get-value account)" \
/tmp/ignition-api &

TOK="$(gcloud auth print-identity-token)"
curl -s -o /dev/null -w '%{http_code}\n' localhost:18080/v1/projects/prj_local/roleBindings           # 401 (no token)
curl -s -H "Authorization: Bearer $TOK" localhost:18080/v1/projects/prj_local/roleBindings | jq .     # 200, you as owner
curl -s -X PUT -H "Authorization: Bearer $TOK" -H 'content-type: application/json' -d '{"role":"developer"}' \
  localhost:18080/v1/projects/prj_local/roleBindings/svc@example.iam.gserviceaccount.com | jq .       # 200
```

Add `IGNITION_OIDC_HOSTED_DOMAINS=<your-domain>` to see a non-Workspace token rejected with 401.

**In-cluster dev.** Recreate the control-plane secret without `DEV_BEARER`, add a bootstrap admin, and roll:

```bash
kubectl -n ignition-system create secret generic ignition-control-plane \
  --from-literal=STREAM_TOKEN_SECRET="$(openssl rand -base64 48)" \
  --from-literal=DATABASE_URL="postgres://ignition:${SQL_PASS}@127.0.0.1:5432/ignition?sslmode=disable" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ignition-system patch configmap ignition-api --type merge -p \
  '{"data":{"IGNITION_BOOTSTRAP_PROJECT":"prj_dev","IGNITION_BOOTSTRAP_ADMIN":"you@your-domain"}}'
kubectl -n ignition-system rollout restart deploy/ignition-api
```

`/statusz` on the admin port shows `auth: oidc: https://accounts.google.com`. Call the API through the port-forward with an impersonated service-account token (works around the fixed `aud` on user credentials):

```bash
gcloud iam service-accounts add-iam-policy-binding "cli@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/iam.serviceAccountTokenCreator --member="user:$(gcloud config get-value account)"
TOK="$(gcloud auth print-identity-token \
  --impersonate-service-account="cli@${PROJECT}.iam.gserviceaccount.com" \
  --audiences="https://api.dev.ignition.dev" --include-email)"
# bind that SA first: psql "$DSN" -v project=prj_dev -v owner_email="cli@${PROJECT}.iam.gserviceaccount.com" -f db/rolebindings.sql
curl -s -H "Authorization: Bearer $TOK" "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes" | jq .
```

Restore dev-bearer by re-adding `DEV_BEARER` to the secret and rolling again.

**Prober** validates the same path continuously on staging: its Workload Identity ID token authenticates as `ignition-prober@<project>.iam.gserviceaccount.com`, which must be bound `developer` (`db/rolebindings.sql`). See [`deploy/PIPELINE.md`](../../deploy/PIPELINE.md).

**IAP** (staging/prod only, needs the HTTPS Ingress): after `terraform apply` with `iap_enabled=true`, capture the backend-service audience, set it in `deploy/k8s/components/iap/enable-iap-config.yaml`, add the component to the overlay, and open `https://api.<env>.ignition.dev/...` in a browser — IAP runs sign-in and the API verifies the `X-Goog-IAP-JWT-Assertion`.

### 7. Optional destructive teardown

Do not run this section merely to complete deployment verification. When the environment is no longer needed, stop the local port-forward, then delete billable compute and database resources. Set `DELETE_ARTIFACTS=true` only if the pushed images are no longer needed. Set `DELETE_SHARED_INFRA=true` only if the VPC will not be reused.

```bash
kill "${PORT_FORWARD_PID}" 2>/dev/null || true
rm -f -- "${PORT_FORWARD_LOG}"
trap - EXIT

gcloud container clusters delete "${CLUSTER}" --region="${REGION}" --quiet
gcloud sql instances delete "${SQL_INSTANCE}" --quiet

if [[ "${DELETE_ARTIFACTS:-false}" == "true" ]]; then
  gcloud artifacts repositories delete "${AR_REPO}" --location="${REGION}" --quiet
  gcloud artifacts repositories delete "${SANDBOX_REPO}" --location="${REGION}" --quiet
fi

if [[ "${DELETE_SHARED_INFRA:-false}" == "true" ]]; then
  gcloud services vpc-peerings delete \
    --service=servicenetworking.googleapis.com --network="${NETWORK}" --quiet
  gcloud compute addresses delete "${PSA_RANGE}" --global --quiet
  for rule in $(gcloud compute firewall-rules list \
    --filter="network=${NETWORK} AND name~^ignition-node-egress" --format='value(name)'); do
    gcloud compute firewall-rules delete "${rule}" --quiet
  done
  gcloud compute routes delete ignition-private-google-apis --quiet
  for zone_domain in googleapis.com pkg.dev gcr.io; do
    zone_name="ignition-${zone_domain//./-}"
    gcloud dns record-sets delete "${zone_domain}." --zone="${zone_name}" --type=A --quiet || true
    gcloud dns record-sets delete "*.${zone_domain}." --zone="${zone_name}" --type=CNAME --quiet || true
    gcloud dns managed-zones delete "${zone_name}" --quiet || true
  done
  gcloud compute routers nats delete "${NAT}" \
    --router="${ROUTER}" --region="${REGION}" --quiet
  gcloud compute routers delete "${ROUTER}" --region="${REGION}" --quiet
  gcloud compute networks subnets delete "${SUBNET}" --region="${REGION}" --quiet
  gcloud compute networks delete "${NETWORK}" --quiet
  for SA in ignition-api ignition-controller "${NODE_SA}"; do
    gcloud iam service-accounts delete \
      "${SA}@${PROJECT}.iam.gserviceaccount.com" --quiet
  done
fi
```

Without the optional flags, the VPC, node-egress firewall rules, the Private Google Access route and DNS zones, NAT, reserved private-services range, service accounts, and Artifact Registry repositories remain for a subsequent cluster. Delete the project instead if it was created only for this run and you want to remove everything: `gcloud projects delete "${PROJECT}"`.

## Verify the implemented sandbox path

Create requires only `imageId` and `Idempotency-Key`; any omitted `resources` / `timeouts` / `network` fields come from the default runtime. The body below pins an `NVIDIA_L4` sandbox with the maximum 600-second startup timeout (a cold L4 node can take several minutes). For a CPU sandbox, drop `resources` entirely.

### CPU quick check (no GPU quota required)

`img_seed` is the single image id the dev bearer seeds; the controller resolves it to `${SANDBOX_REPO}/img_seed`, so step 5 must push the **CPU** seed variant there when this is the path under test. A bare body lands on the default (CPU-only) runtime and the `cpu-sandbox` pool; readiness is kubelet PodReady alone — no `ignition-gpu-agent` attestation gate.

```bash
export CREATE_KEY="cpu-$(date +%s)"
export SBX="$(curl --fail-with-body -sS -X POST \
  "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes" \
  -H "Authorization: Bearer dev-only-token" \
  -H "Idempotency-Key: ${CREATE_KEY}" \
  -H "Content-Type: application/json" \
  --data '{"imageId":"img_seed"}' | jq -er '.sandbox.id')"

for attempt in $(seq 1 60); do
  STATE="$(curl --fail-with-body -sS -H "Authorization: Bearer dev-only-token" \
    "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes/${SBX}" | jq -er '.state,.stateReason' | paste -sd' ')"
  echo "${STATE}"
  case "${STATE%% *}" in READY|FAILED|FINISHED) break;; esac
  sleep 5
done

curl --fail-with-body -sS -X POST \
  "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes/${SBX}:terminate" \
  -H "Authorization: Bearer dev-only-token" -H "Idempotency-Key: term-$(date +%s)" | jq .
```

`FAILED / CAPACITY_UNAVAILABLE` here means the pool's `FailedScaleUp: GCE out of resources` — a zonal machine-type stockout in `${ZONE}`, **not** a quota problem (confirm with `gcloud compute regions describe "${REGION}"` — the relevant `*_CPUS` metric shows headroom). The control plane surfaces it correctly and cleans up the pending Pod. Recreate the pool on an available type and retry:

```bash
gcloud container node-pools delete cpu-sandbox --cluster="${CLUSTER}" --region="${REGION}" --quiet
CPU_SANDBOX_MACHINE=e2-standard-8   # or another available gVisor-capable type
# ...re-run the `gcloud container node-pools create cpu-sandbox` block from step 2
```

```bash
export CREATE_KEY="create-$(date +%s)"
export CREATE_BODY='{"imageId":"img_seed","resources":{"cpuMilli":1000,"memoryMiB":2048,"accelerator":{"count":1,"type":"NVIDIA_L4"}},"timeouts":{"startupSeconds":600}}'
export CREATE_RESPONSE="$(curl --fail-with-body -sS -X POST \
  "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes" \
  -H "Authorization: Bearer dev-only-token" \
  -H "Idempotency-Key: ${CREATE_KEY}" \
  -H "Content-Type: application/json" \
  --data "${CREATE_BODY}")"

export SBX="$(jq -er '.sandbox.id' <<<"${CREATE_RESPONSE}")"
export OPERATION="$(jq -er '.operation.id' <<<"${CREATE_RESPONSE}")"
jq . <<<"${CREATE_RESPONSE}"
printf 'sandbox=%s operation=%s\n' "${SBX}" "${OPERATION}"

# Same key and body must replay the same sandbox.
export REPLAYED_SBX="$(curl --fail-with-body -sS -X POST \
  "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes" \
  -H "Authorization: Bearer dev-only-token" \
  -H "Idempotency-Key: ${CREATE_KEY}" \
  -H "Content-Type: application/json" \
  --data "${CREATE_BODY}" | jq -er '.sandbox.id')"
[[ "${REPLAYED_SBX}" == "${SBX}" ]]

export SANDBOX_STATE=CREATING
for attempt in $(seq 1 120); do
  export SANDBOX_JSON="$(curl --fail-with-body -sS \
    -H "Authorization: Bearer dev-only-token" \
    "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes/${SBX}")"
  export SANDBOX_STATE="$(jq -er '.state' <<<"${SANDBOX_JSON}")"
  printf 'state=%s reason=%s\n' \
    "${SANDBOX_STATE}" "$(jq -r '.stateReason' <<<"${SANDBOX_JSON}")"
  [[ "${SANDBOX_STATE}" == "FAILED" ]] && break
  [[ "${SANDBOX_STATE}" == "READY" ]] && break
  sleep 5
done
[[ "${SANDBOX_STATE}" == "READY" ]]
jq . <<<"${SANDBOX_JSON}"

export POD_NAME="${SBX/_/-}"
kubectl -n ignition-sandboxes get events \
  --field-selector="involvedObject.name=${POD_NAME}" \
  --sort-by='.lastTimestamp'

# Release the GPU test sandbox so the zero-minimum node pool can scale down.
export TERMINATE_KEY="terminate-$(date +%s)"
curl --fail-with-body -sS -X POST \
  "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes/${SBX}:terminate" \
  -H "Authorization: Bearer dev-only-token" \
  -H "Idempotency-Key: ${TERMINATE_KEY}" | jq .

for attempt in $(seq 1 60); do
  export SANDBOX_STATE="$(curl --fail-with-body -sS \
    -H "Authorization: Bearer dev-only-token" \
    "http://127.0.0.1:${LOCAL_API_PORT}/v1/projects/prj_dev/sandboxes/${SBX}" | jq -er '.state')"
  [[ "${SANDBOX_STATE}" == "FINISHED" ]] && break
  sleep 5
done
[[ "${SANDBOX_STATE}" == "FINISHED" ]]
```

This is the acceptance boundary today. The controller creates and schedules the GPU Pod. `sandbox-init` `/readyz` passes once its local probe (device nodes + `nvidia-smi` + `cuInit()`) succeeds, and `ignition-gpu-agent` independently stamps the canonical `ignition.io/gpu-uuid` + `ignition.io/init-healthy`; the controller advances the sandbox to `READY` only when both hold. If the cold node does not arrive within 600 seconds, inspect the retained events. `FailedScaleUp` with quota exceeded means either the regional L4 or global all-regions GPU quota is insufficient; `CAPACITY_UNAVAILABLE` is the expected public infrastructure failure. Terminating the test sandbox removes the Pod and makes the GPU node eligible for autoscaler scale-down.

Do **not** manually add readiness annotations. The sandbox Pod has no Kubernetes token; the GPU attestation annotations come only from `ignition-gpu-agent`, and kubelet PodReady only from probing `sandbox-init`. Process execution and attach verification are not built — they require `ignition-gateway` and `sandbox-init` process supervision, neither of which is shipped.

## What not to do

- Do not put `ignition-api` and customer sandboxes on the same node pool.
- Do not give the API ServiceAccount Pod RBAC.
- Do not give sandbox Pods Workload Identity or a usable metadata identity.
- Do not enable public IPs on the GPU pool.
- Do not put an internet-enabled sandbox pool on the `ignition-node` tag or restricted subnet; that tag's egress is default-deny and Google-only. Internet-enabled pods need their own node pool, subnet, tag, and NAT-scoped network profile.
- Do not share a GPU node across two customer sandboxes (no time-sharing, MPS, or MIG).
- Do not use Autopilot; this pool shape is **GKE Standard**.
- Do not implement `network.internetAccess` by translating client input into arbitrary Kubernetes NetworkPolicy rules.
- Do not run nodes as the default Compute Engine service account.
- Do not treat node provision time as in-SLO; keep a warm buffer (`min_warm` 1–2) before measuring the 9s path.
- Do not manage a GCP resource with both `gcloud` and Terraform.

## Not covered here

Not built: Project/Image/Secret/Event APIs, digest-pinned images, the `ignition-gateway` image and process exec transport, `ignitionctl`, and the custom Compute Engine worker runtime. Designs: [Client API](../design/ignition-design-client-api-identity.md), [Data plane](../design/ignition-design-data-plane-networking.md).

| Item | Value |
|---|---|
| Region | `us-central1` |
| Machine (GPU sandbox) | `g2-standard-8` + 1× `nvidia-l4` |
| Machine (CPU sandbox) | `e2-standard-8` (any available gVisor-capable type) |
| RuntimeClass | `gvisor` |
| Taint (GPU / CPU) | `ignition.io/gpu-sandbox=true:NoSchedule` / `ignition.io/sandbox=true:NoSchedule` |
| Label (GPU / CPU) | `ignition.io/node-pool=gpu-sandbox-l4` / `ignition.io/node-pool=cpu-sandbox`; internet-enabled pools append `-internet` |
| Namespaces | `ignition-system`, `ignition-sandboxes` |
| Datapath | Dataplane V2 (`ADVANCED_DATAPATH`) for cluster-owned defense in depth |
| Node egress | restricted tag `ignition-node`: default-deny except cluster, control plane, PGA VIP `199.36.153.8/30`, Cloud SQL `:3307`; internet tag `ignition-sandbox-internet`: deny private/link-local, allow public via NAT |
| Overlay | `deploy/k8s/overlays/dev` |
| Warm SLO | p95 API-to-`READY` ≤ 9s (pre-warmed nodes only) |
