# Ignition implementation guide

**Status:** Matches the current binaries (`cmd/ignition-api`, `cmd/ignition-controller`)  
**Audience:** engineers building and operating the first slice  
**Architecture:** [GKE Sandbox MVP](../design/ignition-design-gke-sandbox.md)  
**API/controller design:** [API and Controller proposal](../design/ignition-design-api-controller.md)  
**Contract:** [Create Sandbox API](../design/ignition-sandbox-create-api.md), [`api/proto/ignition/v1/`](../../api/proto/ignition/v1/)

This is the **only** build-and-deploy runbook: one regional GKE **dev** environment in one GCP project. Commands are bash and target Cloud Shell or another Linux shell. Run every block in the same shell unless the text says otherwise. Architecture stays in `docs/design/`. Overlay: `deploy/k8s/overlays/dev`.

`gcloud` here is the bootstrap. Terraform can replace the GCP half later (`deploy/terraform/README.md`). Do not create the same cluster with both.

## What you ship

| Ship | Do not ship yet |
|---|---|
| `SandboxService` (lifecycle + process) and `OperationService` | Custom GCE MIG workers |
| OIDC, Cloud SQL, `ignition-controller` | Digest-pinned images |
| HTTP/JSON public edge (SSE for watch); `sandbox-init` health/GPU readiness | `ignition-gateway` Dockerfile / Ingress; process exec transport |
| `secretRefs` (Secret Manager → Pod env), binary outbound internet preference | GCP network-profile provisioning for both internet modes |

Public transport is **HTTP/JSON**. Protobuf is the schema; JSON field names follow proto `json_name` (lowerCamelCase). `ignition-api` must **not** call Kubernetes. The controller is the only Pod RBAC identity. They meet in Cloud SQL.

## Prerequisites

- GCP project with billing. GPU capacity requires both regional **NVIDIA L4 quota** (`NVIDIA_L4_GPUS`) and global all-regions GPU quota (`GPUS_ALL_REGIONS`).
- Go 1.26.7+, Docker, `gcloud`, `gke-gcloud-auth-plugin`, `kubectl`, `jq`, `curl`, and `openssl`. `buf` is required only if you change protos (`buf lint` works; `buf generate` is not wired — there is no `buf.gen.yaml` yet).
- This overlay uses `IGNITION_DEV_BEARER`. Real OIDC (RS256 + JWKS) is required if you later set `IGNITION_ENV` to staging or prod.
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

Require `Authorization: Bearer <access JWT>` on every route except `GET /healthz`. Require `Idempotency-Key` (max 128 bytes) on create, terminate, cancel, and process create/attach/signal/cancel.

### Auth

Production validates RFC 9068 access JWTs over **RS256 + JWKS** (not the attach stream HMAC): exact issuer (`IGNITION_OIDC_ISSUER`) and audience (`IGNITION_OIDC_AUDIENCE`); `typ=at+jwt`; RS256; `exp` / `iat` (and `nbf` when present) with ≤ 60s skew; JWKS from `IGNITION_OIDC_JWKS_URL` or `{issuer}/.well-known/openid-configuration` → `jwks_uri`.

Then check project RBAC for `sandbox.create` / `sandbox.get` / `sandbox.terminate` / `sandbox.exec` / `process.get` / `operation.get` / `operation.cancel`.

- no/invalid bearer → `401`
- no role binding, unknown IDs, **and** in-project deny of terminate/cancel → `404`
- in-project deny of create/exec (viewer) → `403`

Until Project APIs exist, seed one project in Cloud SQL and bind the OIDC subject in `role_bindings`. This overlay sets `IGNITION_DEV_BEARER` (forbidden when `IGNITION_ENV` is staging/prod).

### Admission and store

Validate before the SQL transaction: `imageId` matches `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`. `resources`, `placement`, `timeouts`, `network` are optional — each unset field is merged from the system default runtime (`IGNITION_DEFAULT_RUNTIME`, built-in = CPU-only) and the resolved `RuntimeSpec` is validated: `resources.accelerator.type` in `IGNITION_ALLOWED_ACCELERATORS` (default `NONE,NVIDIA_L4`), `count` `1` for `NVIDIA_L4` / `0` for `NONE`; CPU ≤ 8000m, memory ≤ 32768 MiB; timeout caps and enum checks in `internal/store/runtime.go`; `placement.computeEnvironment` is `STANDARD` or `BARE_METAL`; `network.internetAccess` is `ENABLED` or `DISABLED`. Command/label caps stay in `internal/api/limits.go`. `GET /v1/projects/{project}/runtimes/default` returns the resolved default runtime.

In one serializable transaction: idempotency key, insert sandbox `CREATING`, insert `CREATE_SANDBOX` operation, increment `project_quota.active`. Return `202`. Same key + same hash replays; different hash → `409 IDEMPOTENCY_KEY_REUSED`. **Do not create a Kubernetes Pod here.**

Get/list are project-scoped SQL. Watch is an SSE snapshot plus heartbeats, then close after ~60s (`Last-Event-ID` is not implemented). Terminate sets desired `TERMINATING` (`202`); permission deny is `404`. `GET /healthz` pings the store (503 if Postgres is down).

Process APIs require `READY`. `attach` is idempotent (same key replays `streamEpoch`). Stream tokens use `IGNITION_STREAM_TOKEN_SECRET`. `signal` allowlist: `SIGTERM`, `SIGINT`, `SIGKILL`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2`. Cancel of a still-`PENDING`/`RUNNING` create fails the sandbox (`CANCELLED`) and releases quota.

Tables (`db/migrations/000001_init.up.sql`, embedded as `internal/store/schema.sql`): `projects`, `role_bindings`, `images`, `sandboxes`, `processes`, `operations`, `idempotency_keys`, `project_quota`, `controller_leases`. `store.Open` (API) migrates; `store.OpenWithoutMigrate` (controller) is DML only.

### Environment variables

| Variable | Who | Notes |
|---|---|---|
| `IGNITION_ENV` | both | this overlay sets `dev`; `staging` / `prod` / `production` fail closed |
| `DATABASE_URL` | both | required when `IGNITION_ENV` is staging/prod |
| `IGNITION_OIDC_ISSUER` | API | required when `IGNITION_ENV` is staging/prod |
| `IGNITION_OIDC_JWKS_URL` | API | optional explicit JWKS |
| `IGNITION_OIDC_AUDIENCE` | API | default `https://api.ignition.dev` |
| `IGNITION_STREAM_TOKEN_SECRET` | API | attach tokens; required non-default in staging/prod |
| `IGNITION_DEV_BEARER` | API | allowed in this overlay |
| `IGNITION_GATEWAY_URL` | API | stream token audience |
| `IGNITION_ALLOWED_ACCELERATORS` | API | AcceleratorType allowlist; default `NONE,NVIDIA_L4` (alias: `IGNITION_ALLOWED_GPU_TYPES`) |
| `IGNITION_DEFAULT_RUNTIME` | API | JSON `RuntimeSpec` merged over the built-in CPU default; validated at startup |
| `IGNITION_MAX_ACTIVE_SANDBOXES` | API | per-project quota |
| `IGNITION_K8S_NAMESPACE` | controller | default `ignition-sandboxes` |
| `IGNITION_MIN_WARM` / `IGNITION_MAX_WARM` | controller | balloon Pods |
| `IGNITION_SANDBOX_IMAGE_PREFIX` | controller | Artifact Registry prefix, e.g. `us-central1-docker.pkg.dev/${PROJECT}/sandboxes` |
| `IGNITION_GCP_PROJECT` | controller | used to compose the image prefix and Secret Manager project |

Kubernetes secret keys: `DATABASE_URL`, `STREAM_TOKEN_SECRET`, `OIDC_ISSUER`, optional `DEV_BEARER`. DSN through the Auth Proxy sidecar: `postgres://ignition:…@127.0.0.1:5432/ignition?sslmode=disable`.

HTTP: `ReadHeaderTimeout` 10s, `IdleTimeout` 90s (no short `WriteTimeout`, so SSE can flush). `X-Request-Id` sanitized to `[A-Za-z0-9._-]`, max 128.

### Controller

Distinct KSA. **This** binary is the only one with Pod RBAC. It must not run DDL.

Loop: list sandboxes; `BARE_METAL` currently fails with `COMPUTE_ENVIRONMENT_UNAVAILABLE` without creating a GKE Pod; an accelerator type with no `internal/k8s` profile fails `WORKLOAD_NOT_SUPPORTED` without a Pod; for `STANDARD`, skip Pod create when already `FAILED`/`FINISHED`/`TERMINATING`; resolve image as `${IGNITION_SANDBOX_IMAGE_PREFIX}/{imageId}` (or `{region}-docker.pkg.dev/${IGNITION_GCP_PROJECT}/sandboxes/{imageId}`) after the charset check (empty/invalid → `IMAGE_UNAVAILABLE`, no Pod); create Pod `sbx-{id}` from the accelerator profile — `runtimeClassName: gvisor` always; `NVIDIA_L4` gets one whole GPU, the `ignition.io/gpu-sandbox` toleration, `gpu-sandbox-l4` node pool, and hostname anti-affinity; `NONE` gets no device, the `ignition.io/sandbox` toleration, and the `cpu-sandbox` node pool — plus **read-only root filesystem**, writable `/scratch`, and `IGNITION_ACCELERATOR` env; inject `secretRefs` from Secret Manager as env (missing secret → `SECRET_UNAVAILABLE`, no Pod); map the stored internet preference to a preconfigured GCP network profile; map Pod conditions → `SCHEDULED` / `STARTED` / `READY` / `FAILED`. `sandbox-init` serves `/healthz` and `/readyz` on port 8081; for `NVIDIA_L4` readiness requires exactly one GPU identity (`NVIDIA_VISIBLE_DEVICES`, NVIDIA procfs, or a single `/dev/nvidiaN`), for `NONE` readiness is the supervisor being up. Kubelet's resulting PodReady condition advances the public state to `READY`. The sandbox receives no Kubernetes credential. On terminate delete the Pod. Annotate occupied GPU nodes with `cluster-autoscaler.kubernetes.io/scale-down-disabled=true`. Balloon scale-in waits 15 minutes. Cordon: GET the node, patch only if `ignition.io/node-pool=gpu-sandbox-l4`, return the error if cordon fails. Never create a second Pod for the same id.

Controller RBAC: Pods in `ignition-sandboxes`; ClusterRole get/list/patch Nodes. No cluster-admin. API KSA has **no** Kubernetes RBAC.

### `ignitionctl`

The binary exists (`cmd/ignitionctl`) but every subcommand returns `not implemented`. Do not use it for this slice; use the `curl` examples below.

---

## Deploy regional dev

One GCP project. Regional private GKE, private-IP Cloud SQL, no public Ingress. Port-forward + `IGNITION_DEV_BEARER`.

```text
Regional GKE control plane (us-central1); workers pinned to us-central1-a
  CPU pool     1–3× e2-standard-4 (total autoscaling)
  GPU pool     0–2× g2-standard-8 + 1× L4 + gVisor (private nodes)
Cloud SQL      zonal PostgreSQL 16, private IP, Auth Proxy --private-ip
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
export ROUTER=ignition-router
export NAT=ignition-nat
export PSA_RANGE=ignition-psa
export MASTER_RANGE=172.16.0.0/28
export NODES_RANGE=10.10.0.0/20
export PODS_RANGE=10.20.0.0/16
export SVCS_RANGE=10.30.0.0/20
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

Private nodes, no public IPs on GPU VMs. Cloud NAT for image pulls. Do **not** enable GPU time-sharing, MPS, or MIG. SQL is **zonal** (one env); the *cluster* is regional.

The cluster is created with **GKE Dataplane V2** (`--enable-dataplane-v2`) for cluster-owned defense in depth. Client networking input is not converted into Kubernetes NetworkPolicy. `network.internetAccess` selects a preconfigured GCP network profile; VPC, subnet, firewall, NAT, and metadata protections are the enforcement boundary.

```bash
gcloud services enable \
  container.googleapis.com compute.googleapis.com sqladmin.googleapis.com \
  artifactregistry.googleapis.com secretmanager.googleapis.com \
  iamcredentials.googleapis.com cloudresourcemanager.googleapis.com \
  servicenetworking.googleapis.com cloudbuild.googleapis.com \
  containerfilesystem.googleapis.com

gcloud compute networks create "${NETWORK}" --subnet-mode=custom
gcloud compute networks subnets create "${SUBNET}" \
  --network="${NETWORK}" --region="${REGION}" --range="${NODES_RANGE}" \
  --secondary-range=pods="${PODS_RANGE}",svcs="${SVCS_RANGE}" \
  --enable-private-ip-google-access

gcloud compute routers create "${ROUTER}" --network="${NETWORK}" --region="${REGION}"
gcloud compute routers nats create "${NAT}" \
  --router="${ROUTER}" --region="${REGION}" \
  --nat-all-subnet-ip-ranges --auto-allocate-nat-external-ips

gcloud compute addresses create "${PSA_RANGE}" --global \
  --purpose=VPC_PEERING --prefix-length=16 --network="${NETWORK}"
gcloud services vpc-peerings connect \
  --service=servicenetworking.googleapis.com \
  --ranges="${PSA_RANGE}" --network="${NETWORK}"

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
  --enable-private-nodes --enable-autorepair --enable-autoupgrade

# CPU sandbox pool: the default runtime is CPU-only, so bare CreateSandbox
# requests land here. gVisor, no accelerator, scale-to-zero.
gcloud container node-pools create cpu-sandbox \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --machine-type=n2-standard-8 \
  --service-account="${NODE_SA}@${PROJECT}.iam.gserviceaccount.com" \
  --scopes=cloud-platform \
  --image-type=COS_CONTAINERD --sandbox=type=gvisor \
  --num-nodes=0 --enable-autoscaling --total-min-nodes=0 --total-max-nodes=3 \
  --disk-type=pd-balanced --disk-size=100 \
  --node-labels=ignition.io/node-pool=cpu-sandbox \
  --node-taints=ignition.io/sandbox=true:NoSchedule \
  --enable-private-nodes --enable-autorepair --enable-autoupgrade

gcloud sql instances create "${SQL_INSTANCE}" \
  --database-version=POSTGRES_16 --edition=ENTERPRISE \
  --tier=db-custom-1-3840 \
  --region="${REGION}" --availability-type=ZONAL \
  --network="${NETWORK}" --no-assign-ip \
  --storage-auto-increase --no-backup
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
gcloud container node-pools describe gpu-sandbox-l4 \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --format='yaml(config.accelerators,config.sandboxConfig,config.serviceAccount,autoscaling)'
gcloud container clusters describe "${CLUSTER}" --region="${REGION}" \
  --format='value(networkConfig.datapathProvider, nodePools[].config.serviceAccount)'
kubectl get runtimeclass gvisor
kubectl -n kube-system get ds anetd
```

`datapathProvider` should read `ADVANCED_DATAPATH` (Dataplane V2). Internet-access enforcement must not depend on client-authored Kubernetes policy.

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

The API defaults `network.internetAccess` to `DISABLED`. The current application no longer creates per-sandbox NetworkPolicy resources and its KSA has no NetworkPolicy RBAC. Before advertising either mode, provision distinct GCP network profiles and verify that `DISABLED` has no public egress while both modes block metadata, Cloud SQL, private control ranges, cross-tenant traffic, and unsolicited ingress. Never silently place a `DISABLED` request onto an internet-enabled profile.


### 4. Bootstrap Cloud SQL

The applications use the password user `ignition`; the Auth Proxy uses the KSA/GSA identity only to open the Cloud SQL connection. The API applies schema on startup, so `ignition` must own the database. Use distinct random passwords for the application and the `postgres` bootstrap user.

```bash
export POSTGRES_PASS="$(openssl rand -hex 24)"
export SQL_PASS="$(openssl rand -hex 24)"

cleanup_db_bootstrap() {
  kubectl -n ignition-system delete pod ignition-db-bootstrap --ignore-not-found --wait=true
  kubectl -n ignition-system delete secret ignition-db-bootstrap --ignore-not-found
}
trap cleanup_db_bootstrap EXIT

gcloud sql users set-password postgres \
  --instance="${SQL_INSTANCE}" --password="${POSTGRES_PASS}"
gcloud sql users create ignition \
  --instance="${SQL_INSTANCE}" --password="${SQL_PASS}"

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
      image: postgres:16
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

### 5. Images, rendered overlay, and control-plane secret

Dockerfiles: `deploy/docker/ignition-api.Dockerfile`, `ignition-controller.Dockerfile`. Do not put the sandbox GPU image in the same Dockerfile. `ignition-gateway` has no Dockerfile yet.

```bash
export AR="${REGION}-docker.pkg.dev/${PROJECT}/${AR_REPO}"
make push-images IMAGE_REGISTRY="${AR}" IMAGE_TAG=dev

export SANDBOX_IMAGE="${REGION}-docker.pkg.dev/${PROJECT}/${SANDBOX_REPO}/img_seed:latest"
docker build -f images/sandbox-init/Dockerfile -t "${SANDBOX_IMAGE}" .
docker push "${SANDBOX_IMAGE}"

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

`deploy/cloudbuild/build.yaml` also runs `go test ./...` and the Postgres store tests. The automated
PR-merge / nightly / noon-staging pipeline built on it is documented in
[`deploy/PIPELINE.md`](../../deploy/PIPELINE.md).

Create or update the control-plane secret declaratively, deploy the rendered overlay, and require both rollouts to complete:

```bash
kubectl -n ignition-system create secret generic ignition-control-plane \
  --from-literal=STREAM_TOKEN_SECRET="$(openssl rand -base64 48)" \
  --from-literal=OIDC_ISSUER="https://dev.invalid/" \
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

Without the optional flags, the VPC, NAT, reserved private-services range, service accounts, and Artifact Registry repositories remain for a subsequent cluster. Delete the project instead if it was created only for this run and you want to remove everything: `gcloud projects delete "${PROJECT}"`.

## Verify the implemented sandbox path

Create requires only `imageId` and `Idempotency-Key`; any omitted `resources` / `timeouts` / `network` fields come from the default runtime. The body below pins an `NVIDIA_L4` sandbox with the maximum 600-second startup timeout (a cold L4 node can take several minutes). For a CPU sandbox, drop `resources` entirely.

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

This is the current acceptance boundary. The controller creates and schedules the GPU Pod. `sandbox-init` becomes ready only after it observes exactly one GPU assignment, so the kubelet readiness probe and controller advance the sandbox to `READY`. If the cold node does not arrive within 600 seconds, inspect the retained events. `FailedScaleUp` with quota exceeded means either the regional L4 or global all-regions GPU quota is insufficient; `CAPACITY_UNAVAILABLE` is the expected public infrastructure failure. Terminating the test sandbox removes the Pod and makes the GPU node eligible for autoscaler scale-down.

Do **not** manually add readiness annotations. The sandbox Pod has no Kubernetes token; readiness comes only from kubelet probing `sandbox-init`. Process execution and attach verification remain the next slice and additionally require `ignition-gateway`, which is not shipped.

## What not to do

- Do not put `ignition-api` and customer sandboxes on the same node pool.
- Do not give the API ServiceAccount Pod RBAC.
- Do not give sandbox Pods Workload Identity or a usable metadata identity.
- Do not enable public IPs on the GPU pool.
- Do not share a GPU node across two customer sandboxes (no time-sharing, MPS, or MIG).
- Do not use Autopilot; this pool shape is **GKE Standard**.
- Do not implement `network.internetAccess` by translating client input into arbitrary Kubernetes NetworkPolicy rules.
- Do not run nodes as the default Compute Engine service account.
- Do not treat node provision time as in-SLO; keep a warm buffer (`min_warm` 1–2) before measuring the 9s path.
- Do not manage a GCP resource with both `gcloud` and Terraform.

## Next slices (out of scope here)

Project, image, and event APIs; digest-pinned images; GCP network profiles for internet-disabled and internet-enabled sandboxes; `ignition-gateway` image; custom Compute Engine workers if GKE cannot meet the SLO. Designs: [Client API](../design/ignition-design-client-api-identity.md), [Data plane](../design/ignition-design-data-plane-networking.md).

| Item | Value |
|---|---|
| Region | `us-central1` |
| Machine (sandbox) | `g2-standard-8` + 1× `nvidia-l4` |
| RuntimeClass | `gvisor` |
| Taint | `ignition.io/gpu-sandbox=true:NoSchedule` |
| Label | `ignition.io/node-pool=gpu-sandbox-l4` |
| Namespaces | `ignition-system`, `ignition-sandboxes` |
| Datapath | Dataplane V2 (`ADVANCED_DATAPATH`) for cluster-owned defense in depth |
| Overlay | `deploy/k8s/overlays/dev` |
| Warm SLO | p95 API-to-`READY` ≤ 9s (pre-warmed nodes only) |
