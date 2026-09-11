#!/usr/bin/env bash
#
# Stand up (or re-apply) one customer's Ignition install: a fresh or existing
# GCP project, real GKE + Cloud SQL infra via Terraform, the control-plane
# images, a rendered deploy/k8s/overlays/sample overlay, and one bootstrap
# owner — using real Google OIDC, not IGNITION_DEV_BEARER.
#
# See docs/guides/customer-onboarding.md for the full walkthrough and how to
# tear this down. Idempotent: re-running against the same CUSTOMER_PROJECT_ID
# reuses the existing project, Terraform state, and locally-persisted
# secrets (deploy/scripts/.onboard-secrets/<project>/, gitignored).
#
# Required environment variables:
#   CUSTOMER_PROJECT_ID  GCP project id. Created if it does not already exist.
#   OPERATOR_CIDR         Your IP as a /32 — allowed to reach the GKE control
#                         plane, e.g. OPERATOR_CIDR="$(curl -s ifconfig.me)/32"
#   BOOTSTRAP_ADMIN       Google account email seeded as the first project's
#                         owner. Must be the identity you run step 8 as, i.e.
#                         `gcloud config get-value account`.
#
# Required only when CUSTOMER_PROJECT_ID does not exist yet:
#   BILLING_ACCOUNT      `gcloud billing accounts list` to find one.
#
# Optional:
#   REGION             default us-central1
#   INSTALL_NAME        default = CUSTOMER_PROJECT_ID; only shapes the OIDC
#                        audience string, e.g. https://api.<INSTALL_NAME>.ignition.dev
#   BOOTSTRAP_PROJECT   default prj_customer — the customer's first Ignition project id
#   GPU_MAX_NODES        default 0 (no GPU node pool; >0 enables L4 and is
#                        checked against real quota before terraform apply)
#
# Example:
#   CUSTOMER_PROJECT_ID=acme-ignition OPERATOR_CIDR="$(curl -s ifconfig.me)/32" \
#   BOOTSTRAP_ADMIN=platform-lead@acme.com BILLING_ACCOUNT=012345-6789AB-CDEF01 \
#     deploy/scripts/onboard.sh

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "${REPO_ROOT}"

: "${CUSTOMER_PROJECT_ID:?set CUSTOMER_PROJECT_ID}"
: "${OPERATOR_CIDR:?set OPERATOR_CIDR, e.g. OPERATOR_CIDR=\"\$(curl -s ifconfig.me)/32\"}"
: "${BOOTSTRAP_ADMIN:?set BOOTSTRAP_ADMIN to the Google account that will own the first project}"
REGION="${REGION:-us-central1}"
INSTALL_NAME="${INSTALL_NAME:-${CUSTOMER_PROJECT_ID}}"
BOOTSTRAP_PROJECT="${BOOTSTRAP_PROJECT:-prj_customer}"
GPU_MAX_NODES="${GPU_MAX_NODES:-0}"

# go: prefer PATH, fall back to the repo-pinned toolchain (matches
# docs/guides/ignition-implementation.md's GO_BIN convention).
if command -v go >/dev/null; then
  GO_BIN="$(command -v go)"
elif [[ -x .tools/go/bin/go ]]; then
  GO_BIN="${REPO_ROOT}/.tools/go/bin/go"
else
  echo "missing required tool: go (and no .tools/go/bin/go fallback)" >&2
  exit 1
fi

for tool in gcloud kubectl terraform docker jq openssl git; do
  command -v "${tool}" >/dev/null || { echo "missing required tool: ${tool}" >&2; exit 1; }
done
command -v gke-gcloud-auth-plugin >/dev/null || {
  echo "missing gke-gcloud-auth-plugin; install it first:" >&2
  echo "  gcloud components install gke-gcloud-auth-plugin" >&2
  exit 1
}

CURRENT_ACCOUNT="$(gcloud config get-value account 2>/dev/null || true)"
if [[ "${CURRENT_ACCOUNT}" != "${BOOTSTRAP_ADMIN}" ]]; then
  echo "warning: active gcloud account (${CURRENT_ACCOUNT:-none}) != BOOTSTRAP_ADMIN (${BOOTSTRAP_ADMIN})." >&2
  echo "step 8's verification token will be minted for ${CURRENT_ACCOUNT:-none} and will not authenticate as the bootstrap owner." >&2
fi

SECRETS_DIR="${REPO_ROOT}/deploy/scripts/.onboard-secrets/${CUSTOMER_PROJECT_ID}"
mkdir -p "${SECRETS_DIR}"

echo "==> [1/8] project ${CUSTOMER_PROJECT_ID}"
if ! gcloud projects describe "${CUSTOMER_PROJECT_ID}" >/dev/null 2>&1; then
  : "${BILLING_ACCOUNT:?project ${CUSTOMER_PROJECT_ID} does not exist; set BILLING_ACCOUNT to create it (gcloud billing accounts list)}"
  gcloud projects create "${CUSTOMER_PROJECT_ID}"
  gcloud billing projects link "${CUSTOMER_PROJECT_ID}" --billing-account="${BILLING_ACCOUNT}"
fi
gcloud billing projects describe "${CUSTOMER_PROJECT_ID}" --format='value(billingEnabled)' | grep -qi true \
  || { echo "billing is not enabled on ${CUSTOMER_PROJECT_ID}" >&2; exit 1; }

echo "==> [2/8] GPU quota preflight"
gcloud services enable compute.googleapis.com --project="${CUSTOMER_PROJECT_ID}" >/dev/null
if [[ "${GPU_MAX_NODES}" -gt 0 ]]; then
  L4_HEADROOM="$(gcloud compute regions describe "${REGION}" --project="${CUSTOMER_PROJECT_ID}" --format=json \
    | jq -r '[.quotas[] | select(.metric=="NVIDIA_L4_GPUS") | (.limit - .usage)][0] // 0 | floor')"
  GLOBAL_HEADROOM="$(gcloud compute project-info describe --project="${CUSTOMER_PROJECT_ID}" --format=json \
    | jq -r '[.quotas[] | select(.metric=="GPUS_ALL_REGIONS") | (.limit - .usage)][0] // 0 | floor')"
  if (( L4_HEADROOM < GPU_MAX_NODES )) || (( GLOBAL_HEADROOM < GPU_MAX_NODES )); then
    echo "insufficient GPU quota: NVIDIA_L4_GPUS headroom=${L4_HEADROOM}, GPUS_ALL_REGIONS headroom=${GLOBAL_HEADROOM}, need ${GPU_MAX_NODES}" >&2
    echo "request quota at https://console.cloud.google.com/iam-admin/quotas?project=${CUSTOMER_PROJECT_ID}" >&2
    exit 1
  fi
fi

echo "==> [3/8] terraform state bucket"
gcloud services enable storage.googleapis.com --project="${CUSTOMER_PROJECT_ID}" >/dev/null
STATE_BUCKET="${CUSTOMER_PROJECT_ID}-ignition-tfstate"
gcloud storage buckets describe "gs://${STATE_BUCKET}" >/dev/null 2>&1 || \
  gcloud storage buckets create "gs://${STATE_BUCKET}" \
    --project="${CUSTOMER_PROJECT_ID}" --location="${REGION}" \
    --uniform-bucket-level-access

SQL_PASSWORD_FILE="${SECRETS_DIR}/sql_password"
# hex, not base64: this value is embedded unescaped into a postgres:// DSN
# below (and by Terraform into the Cloud SQL user). base64's +, /, = are
# not URL-safe there — a stray "/" in a base64 password gets parsed as a
# path separator, breaking the DSN with "invalid port ... after host"
# (caught live). Hex has no such characters, so no encoding is needed.
[[ -f "${SQL_PASSWORD_FILE}" ]] || { umask 077; openssl rand -hex 32 > "${SQL_PASSWORD_FILE}"; }
SQL_PASSWORD="$(<"${SQL_PASSWORD_FILE}")"

TFVARS="${REPO_ROOT}/deploy/terraform/${CUSTOMER_PROJECT_ID}.tfvars"
[[ -f "${TFVARS}" ]] || cat > "${TFVARS}" <<EOF
project_id    = "${CUSTOMER_PROJECT_ID}"
region        = "${REGION}"
operator_cidr = "${OPERATOR_CIDR}"
gpu_max_nodes = ${GPU_MAX_NODES}
EOF

echo "==> [4/8] terraform apply"
( cd deploy/terraform && \
  terraform init -reconfigure \
    -backend-config="bucket=${STATE_BUCKET}" \
    -backend-config="prefix=ignition" && \
  terraform apply -auto-approve \
    -var-file="${CUSTOMER_PROJECT_ID}.tfvars" \
    -var "sql_password=${SQL_PASSWORD}" && \
  terraform output -json > "${SECRETS_DIR}/tf-outputs.json" )

TF_OUT="${SECRETS_DIR}/tf-outputs.json"
tfout() { jq -r ".${1}.value" "${TF_OUT}"; }
CLUSTER_NAME="$(tfout cluster_name)"
CLUSTER_REGION="$(tfout cluster_region)"
CONTROL_PLANE_REGISTRY="$(tfout control_plane_registry)"
SANDBOX_REGISTRY="$(tfout sandbox_registry)"
SQL_CONNECTION_NAME="$(tfout sql_connection_name)"

echo "==> [5/8] build + push control-plane images"
SHORT_SHA="$(git rev-parse --short=7 HEAD)"
gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet
make push-images IMAGE_REGISTRY="${CONTROL_PLANE_REGISTRY}" IMAGE_TAG="${SHORT_SHA}"

echo "==> [6/8] build + push sandbox seed image"
SANDBOX_IMAGE="${SANDBOX_REGISTRY}/img_seed:latest"
docker build -f images/sandbox-init/Dockerfile -t "${SANDBOX_IMAGE}" .
docker push "${SANDBOX_IMAGE}"

echo "==> [7/8] render + deploy overlay"
RENDER_DIR="$(mktemp -d)"
cleanup() { rm -rf "${RENDER_DIR}"; [[ -n "${API_PID:-}" ]] && kill "${API_PID}" 2>/dev/null || true; [[ -n "${GATEWAY_PID:-}" ]] && kill "${GATEWAY_PID}" 2>/dev/null || true; }
trap cleanup EXIT

cp -R deploy/k8s "${RENDER_DIR}/k8s"
OVERLAY="${RENDER_DIR}/k8s/overlays/sample"

# Tokens are wrapped (__ONBOARD_X__) rather than bare words like PROJECT or
# BOOTSTRAP_ADMIN: a bare-word sed pattern also matches inside unrelated YAML
# *keys* that happen to contain the same substring (IGNITION_GCP_PROJECT,
# IGNITION_BOOTSTRAP_PROJECT, IGNITION_BOOTSTRAP_ADMIN), corrupting the key
# name itself — caught live corrupting the ignition-api ConfigMap into an
# invalid key "IGNITION_<admin-email>". The post-substitution leftover-check
# below can't catch that class of bug either, since the bare word *is* fully
# consumed — it's just consumed in the wrong place.
declare -A SUBS=(
  [__ONBOARD_PROJECT__]="${CUSTOMER_PROJECT_ID}"
  [__ONBOARD_INSTALL__]="${INSTALL_NAME}"
  [__ONBOARD_BOOTSTRAP_PROJECT__]="${BOOTSTRAP_PROJECT}"
  [__ONBOARD_BOOTSTRAP_ADMIN__]="${BOOTSTRAP_ADMIN}"
  [__ONBOARD_SHORT_SHA__]="${SHORT_SHA}"
)
for file in kustomization config serviceaccount-wi cloud-sql-instance; do
  for key in "${!SUBS[@]}"; do
    sed -i "s#${key}#${SUBS[${key}]}#g" "${OVERLAY}/${file}.yaml"
  done
done
for key in "${!SUBS[@]}"; do
  if grep -Rq "${key}" "${OVERLAY}"; then
    echo "unrendered ${key} placeholder remains in ${OVERLAY}" >&2
    exit 1
  fi
done
kubectl kustomize "${OVERLAY}" >/dev/null   # validate before touching the cluster

gcloud container clusters get-credentials "${CLUSTER_NAME}" --region="${CLUSTER_REGION}" --project="${CUSTOMER_PROJECT_ID}"

kubectl get namespace ignition-system >/dev/null 2>&1 || kubectl apply -f "${RENDER_DIR}/k8s/base/namespaces.yaml"

# Cluster-scoped, so no overlay owns them: internal/k8s/types.go hardcodes
# PrioritySandbox="ignition-sandbox" (and PriorityBalloon) onto every
# sandbox Pod the controller builds. Without these the controller can
# never create a single sandbox Pod ("... is forbidden: no PriorityClass
# with name ignition-sandbox was found") -- caught live: a CreateSandbox
# call was admitted and sat in CREATING past its startup timeout with no
# Pod ever created, and no state transition to FAILED either, because the
# reconcile loop retries pod-create indefinitely rather than surfacing the
# scheduling-prerequisite error to the sandbox's state. Values match the
# implementation guide's manual runbook (docs/guides/ignition-implementation.md).
kubectl apply -f - <<'PRIORITYCLASSES'
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
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: { name: ignition-infra-critical }
value: 1000000
globalDefault: false
PRIORITYCLASSES

STREAM_SECRET_FILE="${SECRETS_DIR}/stream_token_secret"
[[ -f "${STREAM_SECRET_FILE}" ]] || { umask 077; openssl rand -base64 48 > "${STREAM_SECRET_FILE}"; }
kubectl -n ignition-system create secret generic ignition-control-plane \
  --from-literal=STREAM_TOKEN_SECRET="$(<"${STREAM_SECRET_FILE}")" \
  --from-literal=DATABASE_URL="postgres://ignition:${SQL_PASSWORD}@127.0.0.1:5432/ignition?sslmode=disable" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -k "${OVERLAY}"
kubectl -n ignition-system rollout status deploy/ignition-api --timeout=5m
kubectl -n ignition-system rollout status deploy/ignition-controller --timeout=5m
kubectl -n ignition-system rollout status deploy/ignition-gateway --timeout=5m

echo "==> [8/8] verify: bootstrap owner + first sandbox"
"${GO_BIN}" build -o "${SECRETS_DIR}/ignitionctl" ./cmd/ignitionctl

kubectl -n ignition-system port-forward --address=127.0.0.1 svc/ignition-api 18080:8080 \
  >"${SECRETS_DIR}/api-port-forward.log" 2>&1 &
API_PID=$!
kubectl -n ignition-system port-forward --address=127.0.0.1 svc/ignition-gateway 8443:8080 \
  >"${SECRETS_DIR}/gateway-port-forward.log" 2>&1 &
GATEWAY_PID=$!

for attempt in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:18080/healthz" >/dev/null && break
  sleep 1
done

# No --audiences: that flag requires impersonating a service account and
# fails outright for a plain human account ("Invalid account type for
# --audiences. Requires valid service account" — hit live). A bare human
# identity token's aud is gcloud's own OAuth client id
# (32555940559.apps.googleusercontent.com), which is exactly why
# IGNITION_OIDC_AUDIENCES on the rendered overlay includes that id — the
# API already accepts this token as-is.
TOKEN="$(gcloud auth print-identity-token)"

# Real image admission (POST .../images resolves sourceRef -> digest); the
# dev-bearer SeedImage shortcut does not run under real OIDC.
curl --fail-with-body -sS -X POST "http://127.0.0.1:18080/v1/projects/${BOOTSTRAP_PROJECT}/images" \
  -H "Authorization: Bearer ${TOKEN}" -H "Content-Type: application/json" \
  --data "{\"imageId\":\"img_seed\",\"sourceRef\":\"${SANDBOX_IMAGE}\"}" | jq .

"${SECRETS_DIR}/ignitionctl" login --server http://127.0.0.1:18080 --token "${TOKEN}" --project "${BOOTSTRAP_PROJECT}"
"${SECRETS_DIR}/ignitionctl" whoami
# --accelerator NONE is required: ignitionctl's default is NVIDIA_L4
# (internal/cli/sandbox.go), not NONE, so omitting it silently requests a
# GPU sandbox -- guaranteed CAPACITY_UNAVAILABLE when GPU_MAX_NODES=0 since
# that node pool can never scale above zero. Caught live: the verification
# sandbox failed with CAPACITY_UNAVAILABLE against a pool sized for zero
# GPU nodes by design.
SBX="$("${SECRETS_DIR}/ignitionctl" sandbox create --image img_seed --accelerator NONE --cpu 1000 --memory 2048 --wait -o json | jq -r '.sandbox.id')"
"${SECRETS_DIR}/ignitionctl" exec "${SBX}" -- true
"${SECRETS_DIR}/ignitionctl" sandbox terminate "${SBX}" --wait

echo
echo "onboarded: project=${CUSTOMER_PROJECT_ID} ignition-project=${BOOTSTRAP_PROJECT} owner=${BOOTSTRAP_ADMIN}"
echo "cluster=${CLUSTER_NAME} sql-connection=${SQL_CONNECTION_NAME}"
echo "next: docs/guides/customer-onboarding.md for ongoing access, teardown, and adding a second project"
