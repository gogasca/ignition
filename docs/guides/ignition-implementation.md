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
| HTTP/JSON public edge (SSE for watch) | `ignition-gateway` Dockerfile / Ingress |
| `secretRefs` (Secret Manager → Pod env), per-sandbox NetworkPolicy | Domain ALLOW_LIST enforcement (CIDRs + DNS only) |

Public transport is **HTTP/JSON**. Protobuf is the schema; JSON field names follow proto `json_name` (lowerCamelCase). `ignition-api` must **not** call Kubernetes. The controller is the only Pod RBAC identity. They meet in Cloud SQL.

## Prerequisites

- GCP project with billing. **NVIDIA L4 quota** in the region (`NVIDIA_L4_GPUS` in `us-central1` unless you decide otherwise).
- Go 1.26.7+, Docker, `gcloud`, `kubectl`, `jq`, `curl`, and `openssl`. `buf` is required only if you change protos (`buf lint` works; `buf generate` is not wired — there is no `buf.gen.yaml` yet).
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

The Dockerfiles use their own pinned Go builder. `GO_BIN` is for local validation. Confirm GKE Sandbox versions and L4 quota before creating resources:

```bash
gcloud container get-server-config --region=us-central1 --format="yaml(channels,validMasterVersions)"
gcloud compute regions describe us-central1 \
  --format="table(quotas.metric,quotas.limit,quotas.usage)" | grep NVIDIA_L4
```

If quota is 0, request it in the console before the GPU node pool.

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

Validate before the SQL transaction: `imageId` matches `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`; `gpu.count` is `1`; `gpu.type` is `NVIDIA_L4` (`IGNITION_ALLOWED_GPU_TYPES`); CPU ≤ 8000m, memory ≤ 32768 MiB; timeout/command/env/label caps in `internal/api/limits.go`; `ALLOW_LIST` needs a TLS domain or CIDR.

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
| `IGNITION_ALLOWED_GPU_TYPES` | API | GpuType allowlist; default `NVIDIA_L4` |
| `IGNITION_MAX_ACTIVE_SANDBOXES` | API | per-project quota |
| `IGNITION_K8S_NAMESPACE` | controller | default `ignition-sandboxes` |
| `IGNITION_MIN_WARM` / `IGNITION_MAX_WARM` | controller | balloon Pods |
| `IGNITION_SANDBOX_IMAGE_PREFIX` | controller | Artifact Registry prefix, e.g. `us-central1-docker.pkg.dev/${PROJECT}/sandboxes` |
| `IGNITION_GCP_PROJECT` | controller | used to compose the image prefix and Secret Manager project |

Kubernetes secret keys: `DATABASE_URL`, `STREAM_TOKEN_SECRET`, `OIDC_ISSUER`, optional `DEV_BEARER`. DSN through the Auth Proxy sidecar: `postgres://ignition:…@127.0.0.1:5432/ignition?sslmode=disable`.

HTTP: `ReadHeaderTimeout` 10s, `IdleTimeout` 90s (no short `WriteTimeout`, so SSE can flush). `X-Request-Id` sanitized to `[A-Za-z0-9._-]`, max 128.

### Controller

Distinct KSA. **This** binary is the only one with Pod RBAC. It must not run DDL.

Loop: list sandboxes; skip Pod create when already `FAILED`/`FINISHED`/`TERMINATING`; resolve image as `${IGNITION_SANDBOX_IMAGE_PREFIX}/{imageId}` (or `{region}-docker.pkg.dev/${IGNITION_GCP_PROJECT}/sandboxes/{imageId}`) after the charset check (empty/invalid → `IMAGE_UNAVAILABLE`, no Pod); create Pod `sbx-{id}` with `runtimeClassName: gvisor`, one GPU, taint, anti-affinity, **read-only root filesystem**, writable `/scratch`; inject `secretRefs` from Secret Manager as env (missing secret → `SECRET_UNAVAILABLE`, no Pod); emit a per-sandbox NetworkPolicy (`ALLOW_LIST` CIDRs + kube-dns; `DENY_ALL` has no extra egress); map Pod conditions → `SCHEDULED` / `STARTED` / `READY` / `FAILED` (`READY` requires init-healthy **and** a GPU UUID annotation — kube `Ready` alone is `STARTED`); on terminate delete the Pod and NetworkPolicy. Annotate occupied GPU nodes with `cluster-autoscaler.kubernetes.io/scale-down-disabled=true`. Balloon scale-in waits 15 minutes. Cordon: GET the node, patch only if `ignition.io/node-pool=gpu-sandbox-l4`, return the error if cordon fails. Never create a second Pod for the same id.

Controller RBAC: Pods and NetworkPolicies in `ignition-sandboxes`; ClusterRole get/list/patch Nodes. No cluster-admin. API KSA has **no** Kubernetes RBAC.

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
export GPU_MAX=2
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

### 2. APIs, VPC, GKE, GPU pool, SQL, AR, IAM

Private nodes, no public IPs on GPU VMs. Cloud NAT for image pulls. Do **not** enable GPU time-sharing, MPS, or MIG. SQL is **zonal** (one env); the *cluster* is regional.

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

gcloud container clusters create "${CLUSTER}" \
  --region="${REGION}" --release-channel=regular \
  --network="${NETWORK}" --subnetwork="${SUBNET}" \
  --cluster-secondary-range-name=pods --services-secondary-range-name=svcs \
  --enable-ip-alias --enable-private-nodes \
  --enable-master-authorized-networks \
  --master-authorized-networks="${OPERATOR_CIDR}" \
  --no-enable-private-endpoint --master-ipv4-cidr="${MASTER_RANGE}" \
  --workload-pool="${PROJECT}.svc.id.goog" --enable-image-streaming \
  --image-type=COS_CONTAINERD \
  --node-locations="${ZONE}" \
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
  --accelerator=type=nvidia-l4,count=1,gpu-driver-version=default \
  --image-type=COS_CONTAINERD --sandbox=type=gvisor \
  --num-nodes=0 --enable-autoscaling --total-min-nodes=0 --total-max-nodes="${GPU_MAX}" \
  --disk-type=pd-balanced --disk-size=100 \
  --node-labels=ignition.io/node-pool=gpu-sandbox-l4 \
  --node-taints=ignition.io/gpu-sandbox=true:NoSchedule \
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
  --role=roles/artifactregistry.reader
gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:ignition-controller@${PROJECT}.iam.gserviceaccount.com" \
  --role=roles/secretmanager.secretAccessor

PROJECT_NUMBER="$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')"
gcloud artifacts repositories add-iam-policy-binding "${AR_REPO}" \
  --location="${REGION}" \
  --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
  --role=roles/artifactregistry.reader
gcloud artifacts repositories add-iam-policy-binding "${SANDBOX_REPO}" \
  --location="${REGION}" \
  --member="serviceAccount:${PROJECT_NUMBER}-compute@developer.gserviceaccount.com" \
  --role=roles/artifactregistry.reader
```

Confirm the cluster, node-pool configuration, and RuntimeClass. If any command fails, stop:

```bash
gcloud container node-pools describe gpu-sandbox-l4 \
  --cluster="${CLUSTER}" --region="${REGION}" \
  --format='yaml(config.accelerators,config.sandboxConfig,autoscaling)'
kubectl get runtimeclass gvisor
```

### 3. Namespaces, identities, PriorityClasses, NetworkPolicy

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
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sandbox-default-deny
  namespace: ignition-sandboxes
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: ignition-system
          podSelector:
            matchLabels:
              app: ignition-gateway
  egress: []
EOF
```

Deny-all sandbox egress matches `network.egress.mode = DENY_ALL`. Per-sandbox `ALLOW_LIST` is a controller-emitted NetworkPolicy selecting `ignition.io/sandbox-id`: kube-dns plus `allowedCidrs`. TLS domains are admitted in SQL; FQDN filtering is still the egress-proxy path, not vanilla NetworkPolicy.

### 4. Bootstrap Cloud SQL

The applications use the password user `ignition`; the Auth Proxy uses the KSA/GSA identity only to open the Cloud SQL connection. The API applies schema on startup, so `ignition` must own the database. Use distinct random passwords for the application and the `postgres` bootstrap user.

```bash
export POSTGRES_PASS="$(openssl rand -hex 24)"
export SQL_PASS="$(openssl rand -hex 24)"

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

kubectl -n ignition-system delete pod ignition-db-bootstrap --wait=true
kubectl -n ignition-system delete secret ignition-db-bootstrap
unset POSTGRES_PASS
```

The temporary Pod runs inside the VPC because a private-IP Cloud SQL instance is not reachable from ordinary Cloud Shell or a laptop without a private network path.

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

This leaves the tracked dev overlay unchanged. As an alternative to `make push-images`, Cloud Build can build the two control-plane images; a manual submission must supply `SHORT_SHA` because that substitution is populated automatically only for triggered builds:

```bash
export SHORT_SHA="$(git rev-parse --short=7 HEAD)"
gcloud builds submit . --config=deploy/cloudbuild.yaml \
  --substitutions="_TAG=dev,_REGION=${REGION},_AR_REPO=${AR_REPO},SHORT_SHA=${SHORT_SHA}"
```

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
kubectl -n ignition-system port-forward svc/ignition-api 8080:8080 \
  >"${PORT_FORWARD_LOG}" 2>&1 &
export PORT_FORWARD_PID=$!
trap 'kill "${PORT_FORWARD_PID}" 2>/dev/null || true' EXIT

for attempt in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:8080/healthz >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail-with-body -sS http://127.0.0.1:8080/healthz
curl --fail-with-body -sS \
  -H "Authorization: Bearer dev-only-token" \
  "http://127.0.0.1:8080/v1/projects/prj_dev/sandboxes" | jq .
```

Then [verify the implemented sandbox path](#verify-the-implemented-sandbox-path). Creating a sandbox scales the L4 pool up to `GPU_MAX`.

### 7. Tear down

Stop the local port-forward, then delete billable compute and database resources. Set `DELETE_ARTIFACTS=true` only if the pushed images are no longer needed. Set `DELETE_SHARED_INFRA=true` only if the VPC will not be reused.

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
  for SA in ignition-api ignition-controller; do
    gcloud iam service-accounts delete \
      "${SA}@${PROJECT}.iam.gserviceaccount.com" --quiet
  done
fi
```

Without the optional flags, the VPC, NAT, reserved private-services range, service accounts, and Artifact Registry repositories remain for a subsequent cluster. Delete the project instead if it was created only for this run and you want to remove everything: `gcloud projects delete "${PROJECT}"`.

## Verify the implemented sandbox path

Create requires `resources` (CPU, memory, `gpu.count=1`, `gpu.type=NVIDIA_L4`) and `Idempotency-Key`. The commands capture IDs instead of requiring manual copy/paste. A cold L4 node can take several minutes, so this request uses the maximum 600-second startup timeout.

```bash
export CREATE_KEY="create-$(date +%s)"
export CREATE_BODY='{"imageId":"img_seed","resources":{"cpuMilli":1000,"memoryMiB":2048,"gpu":{"count":1,"type":"NVIDIA_L4"}},"timeouts":{"startupSeconds":600}}'
export CREATE_RESPONSE="$(curl --fail-with-body -sS -X POST \
  "http://127.0.0.1:8080/v1/projects/prj_dev/sandboxes" \
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
  "http://127.0.0.1:8080/v1/projects/prj_dev/sandboxes" \
  -H "Authorization: Bearer dev-only-token" \
  -H "Idempotency-Key: ${CREATE_KEY}" \
  -H "Content-Type: application/json" \
  --data "${CREATE_BODY}" | jq -er '.sandbox.id')"
[[ "${REPLAYED_SBX}" == "${SBX}" ]]

export SANDBOX_STATE=CREATING
for attempt in $(seq 1 120); do
  export SANDBOX_JSON="$(curl --fail-with-body -sS \
    -H "Authorization: Bearer dev-only-token" \
    "http://127.0.0.1:8080/v1/projects/prj_dev/sandboxes/${SBX}")"
  export SANDBOX_STATE="$(jq -er '.state' <<<"${SANDBOX_JSON}")"
  printf 'state=%s reason=%s\n' \
    "${SANDBOX_STATE}" "$(jq -r '.stateReason' <<<"${SANDBOX_JSON}")"
  [[ "${SANDBOX_STATE}" == "FAILED" ]] && break
  [[ "${SANDBOX_STATE}" == "READY" ]] && { echo "unexpected READY from stub sandbox-init" >&2; exit 1; }
  sleep 5
done
[[ "${SANDBOX_STATE}" == "FAILED" ]]
jq . <<<"${SANDBOX_JSON}"
export SANDBOX_REASON="$(jq -er '.stateReason' <<<"${SANDBOX_JSON}")"
case "${SANDBOX_REASON}" in
  WORKER_LOST|CAPACITY_UNAVAILABLE) ;;
  *) echo "unexpected sandbox failure: ${SANDBOX_REASON}" >&2; exit 1 ;;
esac

export POD_NAME="${SBX/_/-}"
kubectl -n ignition-sandboxes get events \
  --field-selector="involvedObject.name=${POD_NAME}" \
  --sort-by='.lastTimestamp'
```

This is the current honest acceptance boundary. The controller should create and schedule the GPU Pod, after which the sandbox becomes `FAILED` with `WORKER_LOST` because `internal/sandboxinit.Run` returns `not implemented`. If the cold node does not arrive within 600 seconds, `CAPACITY_UNAVAILABLE` is also an expected infrastructure outcome. The controller deletes the failed Pod, so the retained Kubernetes events are the inspection record.

Do **not** manually add `ignition.io/init-healthy` or `ignition.io/gpu-uuid` annotations to simulate `READY`. Real `READY`, process execution, and attach verification require a working `sandbox-init`; attach additionally requires `ignition-gateway`, which is not shipped.

## What not to do

- Do not put `ignition-api` and customer sandboxes on the same node pool.
- Do not give the API ServiceAccount Pod RBAC.
- Do not give sandbox Pods Workload Identity or a usable metadata identity.
- Do not enable public IPs on the GPU pool.
- Do not share a GPU node across two customer sandboxes (no time-sharing, MPS, or MIG).
- Do not use Autopilot; this pool shape is **GKE Standard**.
- Do not treat node provision time as in-SLO; keep a warm buffer (`min_warm` 1–2) before measuring the 9s path.
- Do not manage a GCP resource with both `gcloud` and Terraform.

## Next slices (out of scope here)

Project, image, and event APIs; digest-pinned images; FQDN ALLOW_LIST via egress proxy; `ignition-gateway` image; custom GCE MIG workers if GKE cannot meet the SLO. Designs: [Client API](../design/ignition-design-client-api-identity.md), [Data plane](../design/ignition-design-data-plane-networking.md).

| Item | Value |
|---|---|
| Region | `us-central1` |
| Machine (sandbox) | `g2-standard-8` + 1× `nvidia-l4` |
| RuntimeClass | `gvisor` |
| Taint | `ignition.io/gpu-sandbox=true:NoSchedule` |
| Label | `ignition.io/node-pool=gpu-sandbox-l4` |
| Namespaces | `ignition-system`, `ignition-sandboxes` |
| Overlay | `deploy/k8s/overlays/dev` |
| Warm SLO | p95 API-to-`READY` ≤ 9s (pre-warmed nodes only) |
