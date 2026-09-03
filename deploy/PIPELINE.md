# Ignition CI/CD pipeline (Cloud Build + Cloud Deploy)

Four automated flows, one GCP project, source in GitHub:

| # | When | What | Config |
|---|---|---|---|
| 0 | **PR to `main`** | Per component: `go vet` + `go test ./...` + `docker build` the affected image. **No push** — validation only. A trigger fires only when the PR changes that binary's code or a package it compiles in. | `deploy/cloudbuild/pr.yaml` + `deploy/cloudbuild/prpaths` |
| 1 | Push to `main` (PR merge) | `go vet` + `go test ./...` + Postgres store tests, then build & push `ignition-api`, `ignition-controller`, `ignition-gateway`, `ignition-prober` tagged `sha-<sha>` and `main` | `deploy/cloudbuild/build.yaml` |
| 2 | Nightly ~02:00 | Same tests + build, tagged `sha-<sha>`, `nightly`, `nightly-YYYYMMDD` | `deploy/cloudbuild/build.yaml` |
| 3 | Daily 12:00 | `go test ./...` gate, then a Cloud Deploy release: resolve `:nightly` → digest, roll onto the staging GKE cluster, run the read-only critical-user-journey probes as the verify gate | `deploy/cloudbuild/deploy-staging.yaml` + `deploy/clouddeploy/pipeline.yaml` + `skaffold.yaml` |

```
PR opened/updated ──▶ ignition-pr-{api,controller,data-plane}  (path-filtered)
                        ─▶ vet + test ─▶ docker build <component>   (no push)
GitHub main ──push──▶ ignition-ci-main  ─▶ test ─▶ build ─▶ push :sha,:main   (all 4)
Scheduler 02:00 ────▶ ignition-nightly  ─▶ test ─▶ build ─▶ push :sha,:nightly,:nightly-DATE
Scheduler 12:00 ────▶ ignition-deploy-staging ─▶ test gate ─▶ gcloud deploy releases create
                                                     └─▶ Cloud Deploy ─▶ render overlays/staging
                                                          ─▶ deploy to ignition-staging ─▶ verify (CUJ probes)
```

Promotion never rebuilds — Cloud Deploy always deploys an immutable digest resolved from `:nightly`.

`ignition-gateway` ("dataplane") is built and pushed in flows 1 and 2 so its digest is tracked, but it
has **no Kubernetes Deployment** and is not part of the staging rollout: `internal/gateway` is still a
stub that exits at startup. Add it to `deploy/k8s/base` and the release `--images` list once implemented.

---

## Prerequisites

Everything below assumes the **staging cluster and its data plane already exist** in this project,
built by following `docs/guides/ignition-implementation.md` → *Deploy regional dev*, with these
substitutions:

- `CLUSTER=ignition-staging`
- `IGNITION_ENV=staging` (staging fails closed — `config.Validate()` requires a real `DATABASE_URL`,
  `IGNITION_OIDC_ISSUER`, and a non-default `IGNITION_STREAM_TOKEN_SECRET`, and forbids `DEV_BEARER`)
- skip the `overlays/dev` apply in step 5 — Cloud Deploy (flow 3) applies `overlays/staging`

Concretely, before wiring the pipeline you need:

- Artifact Registry docker repo **`ignition`** in `us-central1` (the dev runbook's `AR_REPO`).
- VPC `ignition-vpc`, private regional cluster `ignition-staging`, private Cloud SQL `ignition-sql`.
- Namespaces + service accounts applied (`kubectl apply -f deploy/k8s/base/namespaces.yaml`,
  `serviceaccounts.yaml`) and the GSAs `ignition-api` / `ignition-controller` with Workload Identity
  bindings and `roles/cloudsql.client`.
- Secret `ignition-control-plane` in namespace `ignition-system` with real keys (`OIDC_ISSUER` must be
  `https://accounts.google.com` if the Workload-Identity prober is used — see the Prober section; do
  **not** add a `DEV_BEARER` key, `config.Validate()` rejects it in staging):
  ```bash
  kubectl -n ignition-system create secret generic ignition-control-plane \
    --from-literal=DATABASE_URL='postgres://ignition:PASS@127.0.0.1:5432/ignition?sslmode=disable' \
    --from-literal=OIDC_ISSUER='https://accounts.google.com' \
    --from-literal=STREAM_TOKEN_SECRET="$(openssl rand -base64 48)"
  ```
- `deploy/k8s/overlays/staging/ingress.yaml`: the managed cert (`api.staging.ignition.dev`) and global
  static IP (`ignition-api-staging`) are manual — reserve the IP and point DNS at it, **or** delete
  `ingress.yaml` from `overlays/staging/kustomization.yaml` for an internal-only staging.

---

## One-time setup

### 0. Variables

```bash
export PROJECT=your-gcp-project
export REGION=us-central1
export NETWORK=ignition-vpc
export CLUSTER=ignition-staging
export GITHUB_OWNER=your-org          # GitHub org/user that owns the repo
export TZ_NAME="Etc/UTC"              # set to your local zone so "noon" is local noon

gcloud config set project "${PROJECT}"
```

### 1. Enable APIs

```bash
gcloud services enable \
  cloudbuild.googleapis.com clouddeploy.googleapis.com \
  artifactregistry.googleapis.com cloudscheduler.googleapis.com \
  container.googleapis.com secretmanager.googleapis.com \
  servicenetworking.googleapis.com iam.googleapis.com
```

### 2. Connect the GitHub repo (Cloud Build 2nd gen)

The GitHub App install needs a browser once:

```bash
gcloud builds connections create github ignition-github --region="${REGION}"
gcloud builds connections describe ignition-github --region="${REGION}" \
  --format='value(installationState.actionUri)'   # open this URL, install on the repo

gcloud builds repositories create ignition \
  --remote-uri="https://github.com/${GITHUB_OWNER}/ignition.git" \
  --connection=ignition-github --region="${REGION}"

export REPO="projects/${PROJECT}/locations/${REGION}/connections/ignition-github/repositories/ignition"
```

### 3. Service accounts and IAM

```bash
# Cloud Build runs builds + creates Cloud Deploy releases
gcloud iam service-accounts create ignition-cloudbuild
CB="ignition-cloudbuild@${PROJECT}.iam.gserviceaccount.com"
for r in roles/logging.logWriter roles/artifactregistry.writer \
         roles/clouddeploy.releaser roles/storage.objectAdmin; do
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${CB}" --role="$r"
done

# Cloud Deploy render/deploy/verify execution
gcloud iam service-accounts create clouddeploy-exec
CD="clouddeploy-exec@${PROJECT}.iam.gserviceaccount.com"
for r in roles/container.developer roles/clouddeploy.jobRunner \
         roles/logging.logWriter roles/artifactregistry.reader; do
  gcloud projects add-iam-policy-binding "${PROJECT}" \
    --member="serviceAccount:${CD}" --role="$r"
done
# Cloud Build must be able to act as the Cloud Deploy execution SA
gcloud iam service-accounts add-iam-policy-binding "${CD}" \
  --member="serviceAccount:${CB}" --role="roles/iam.serviceAccountUser"

# Cloud Scheduler invokes the manual triggers
gcloud iam service-accounts create ignition-scheduler
SCH="ignition-scheduler@${PROJECT}.iam.gserviceaccount.com"
gcloud projects add-iam-policy-binding "${PROJECT}" \
  --member="serviceAccount:${SCH}" --role="roles/cloudbuild.builds.editor"

# Cloud Deploy service agent needs to use the execution SA
gcloud beta services identity create --service=clouddeploy.googleapis.com --project="${PROJECT}"
gcloud iam service-accounts add-iam-policy-binding "${CD}" \
  --member="serviceAccount:service-$(gcloud projects describe ${PROJECT} --format='value(projectNumber)')@gcp-sa-clouddeploy.iam.gserviceaccount.com" \
  --role="roles/iam.serviceAccountUser"
```

Grant the Cloud Build SA and the Cloud Deploy execution SA pull access on the cluster's node SA / GKE
as needed (`roles/container.developer` on `${CD}` above covers `kubectl apply`).

### 4. Private Cloud Build worker pool (staging control plane is private)

Cloud Deploy RENDER/DEPLOY/VERIFY must reach the private GKE endpoint, so they run on a VPC-peered pool.

```bash
gcloud compute addresses create ignition-deploy-pool-range \
  --global --purpose=VPC_PEERING --prefix-length=23 --network="${NETWORK}"

# If the dev runbook already created a servicenetworking peering (ignition-psa),
# update it to include both ranges instead of `connect`:
gcloud services vpc-peerings update --service=servicenetworking.googleapis.com \
  --network="${NETWORK}" --ranges=ignition-psa,ignition-deploy-pool-range --force
# (first time only: gcloud services vpc-peerings connect ... --ranges=ignition-deploy-pool-range)

gcloud builds worker-pools create ignition-deploy-pool \
  --region="${REGION}" \
  --peered-network="projects/${PROJECT}/global/networks/${NETWORK}" \
  --worker-machine-type=e2-medium

# Authorize the pool range on the cluster control plane
POOL_CIDR="$(gcloud compute addresses describe ignition-deploy-pool-range --global \
  --format='value(address)')/23"
EXISTING="$(gcloud container clusters describe "${CLUSTER}" --region="${REGION}" \
  --format='value[separator=","](masterAuthorizedNetworksConfig.cidrBlocks[].cidrBlock)')"
gcloud container clusters update "${CLUSTER}" --region="${REGION}" \
  --enable-master-authorized-networks \
  --master-authorized-networks="${EXISTING:+${EXISTING},}${POOL_CIDR}"
```

### 5. Cloud Deploy delivery pipeline

```bash
sed "s/PROJECT_ID/${PROJECT}/g" deploy/clouddeploy/pipeline.yaml > /tmp/ignition-pipeline.yaml
gcloud deploy apply --file=/tmp/ignition-pipeline.yaml \
  --region="${REGION}" --project="${PROJECT}"
```

### 6. Cloud Build triggers

```bash
# Flow 0 — per-component PR builds. One trigger per container; each fires only
# when the PR touches that binary's code or a package it compiles in. The
# --included-files globs are the single source of truth in
# deploy/cloudbuild/prpaths/paths.go and are verified by its test — regenerate
# with:  go run ./deploy/cloudbuild/prpaths/print   (or copy from paths.go)
for pair in "api:ignition-pr-api" "controller:ignition-pr-controller" "gateway:ignition-pr-data-plane"; do
  comp="${pair%%:*}"; name="${pair##*:}"
  paths="$(go run ./deploy/cloudbuild/prpaths/print "$comp")"
  gcloud builds triggers create github \
    --name="$name" --region="${REGION}" \
    --repository="${REPO}" --pull-request-pattern='^main$' \
    --build-config=deploy/cloudbuild/pr.yaml \
    --included-files="$paths" \
    --service-account="projects/${PROJECT}/serviceAccounts/${CB}" \
    --substitutions="_COMPONENT=${comp}"
done

# Flow 1 — PR merge → main
gcloud builds triggers create github \
  --name=ignition-ci-main --region="${REGION}" \
  --repository="${REPO}" --branch-pattern='^main$' \
  --build-config=deploy/cloudbuild/build.yaml \
  --service-account="projects/${PROJECT}/serviceAccounts/${CB}" \
  --substitutions=_EXTRA_TAG=main

# Flow 2 — nightly build (invoked by Scheduler)
gcloud builds triggers create manual \
  --name=ignition-nightly --region="${REGION}" \
  --source-to-build-repository="${REPO}" --source-to-build-ref=refs/heads/main \
  --git-file-source-repository="${REPO}" --git-file-source-ref=refs/heads/main \
  --git-file-source-path=deploy/cloudbuild/build.yaml \
  --service-account="projects/${PROJECT}/serviceAccounts/${CB}" \
  --substitutions=_EXTRA_TAG=nightly,_DATE_TAG=true

# Flow 3 — noon staging deploy (invoked by Scheduler)
gcloud builds triggers create manual \
  --name=ignition-deploy-staging --region="${REGION}" \
  --source-to-build-repository="${REPO}" --source-to-build-ref=refs/heads/main \
  --git-file-source-repository="${REPO}" --git-file-source-ref=refs/heads/main \
  --git-file-source-path=deploy/cloudbuild/deploy-staging.yaml \
  --service-account="projects/${PROJECT}/serviceAccounts/${CB}"
```

### 7. Cloud Scheduler jobs

```bash
TRIG_URL="https://cloudbuild.googleapis.com/v1/projects/${PROJECT}/locations/${REGION}/triggers"

gcloud scheduler jobs create http ignition-nightly-build \
  --location="${REGION}" --schedule="0 2 * * *" --time-zone="${TZ_NAME}" \
  --uri="${TRIG_URL}/ignition-nightly:run" --http-method=POST \
  --oauth-service-account-email="${SCH}" --message-body='{}'

gcloud scheduler jobs create http ignition-deploy-staging \
  --location="${REGION}" --schedule="0 12 * * *" --time-zone="${TZ_NAME}" \
  --uri="${TRIG_URL}/ignition-deploy-staging:run" --http-method=POST \
  --oauth-service-account-email="${SCH}" --message-body='{}'
```

---

## First run and operations

```bash
# Prove the build path (also seeds :nightly so the deploy has something to resolve)
gcloud builds submit . --config=deploy/cloudbuild/build.yaml --region="${REGION}" \
  --substitutions=_EXTRA_TAG=nightly,_DATE_TAG=true,SHORT_SHA=$(git rev-parse --short HEAD)

# Prove the deploy path
gcloud scheduler jobs run ignition-deploy-staging --location="${REGION}"
gcloud deploy delivery-pipelines describe ignition-staging --region="${REGION}"

# Watch a rollout
gcloud deploy rollouts list --delivery-pipeline=ignition-staging --release=<rel> --region="${REGION}"

# Roll back staging to the previous release
gcloud deploy targets rollback staging \
  --delivery-pipeline=ignition-staging --region="${REGION}"
```

## Prober (synthetic critical-user-journey monitoring)

`ignition-prober` runs the public-API critical user journeys — health, auth guard, default runtime,
list, the `create → ready → list → terminate → finished` sandbox lifecycle, the exec control plane
(`create process → attach token → signal → cancel`), and idempotency replay/conflict — and exports
Prometheus metrics (`ignition_probe_*`) on `:9102`. (Exec byte-streaming and in-sandbox process
supervision ship later; the `process-exec` journey asserts the control-plane surface that exists today
and starts checking the `RUNNING` transition automatically once supervision lands.) It is deployed to staging by `deploy/k8s/components/prober`
(a `Deployment` + `Service`, wired into `overlays/staging`). The Cloud Deploy `verify` gate additionally
runs the read-only subset once per rollout (`skaffold.yaml`, `IGNITION_PROBE_JOURNEYS=lite`,
`IGNITION_PROBE_AUTH=none`).

The continuous prober authenticates with a **Workload Identity ID token** (no secrets). For that to
work on staging:

1. **Staging API OIDC must trust Google.** `IGNITION_OIDC_ISSUER=https://accounts.google.com` and
   `IGNITION_OIDC_SUBJECT_CLAIM=email` are set in the base `ignition-api` **ConfigMap** - no secret
   key. The API audience is `https://api.staging.ignition.dev` (`overlays/staging/config.yaml`); the
   prober ConfigMap's `IGNITION_PROBE_AUDIENCE` (same file) must equal it. The prober now fails to
   start if that variable is unset.

2. **Prober GSA + Workload Identity binding** (no project-level roles — it only mints its own token).
   Terraform does this (`google_service_account.prober` + `prober_wi`); by hand:
   ```bash
   gcloud iam service-accounts create ignition-prober
   PRB="ignition-prober@${PROJECT}.iam.gserviceaccount.com"
   gcloud iam service-accounts add-iam-policy-binding "${PRB}" \
     --role=roles/iam.workloadIdentityUser \
     --member="serviceAccount:${PROJECT}.svc.id.goog[ignition-system/ignition-prober]"
   ```

3. **Authorize the prober in the product store.** With `IGNITION_OIDC_SUBJECT_CLAIM=email` the RBAC
   subject is the GSA email itself. Bind it as `developer` on the probe project so the lifecycle
   journey can create + exec + terminate (own):
   ```bash
   # against the staging DB via the cloud-sql-proxy:
   psql "$STAGING_DSN" -v project=prj_dev -v prober_sa="$PRB" -f db/rolebindings.sql
   #   -> INSERT INTO role_bindings VALUES ('prj_dev', 'ignition-prober@...', 'developer')
   ```

4. **Seed the probe image.** `img_seed` must be pushed to the staging sandboxes repo
   (`${REGION}-docker.pkg.dev/${PROJECT}/sandboxes/img_seed:latest`, see the implementation guide
   step 5) and marked ready:
   ```sql
   INSERT INTO images (project_id, image_id, state) VALUES ('prj_dev', 'img_seed', 'READY');
   ```

Local one-shot smoke against a dev deployment (dev bearer):

```bash
IGNITION_PROBE_TARGET=http://127.0.0.1:18080 IGNITION_PROBE_AUTH=static \
IGNITION_PROBE_TOKEN=dev-only-token IGNITION_PROBE_ONESHOT=true \
go run ./cmd/ignition-prober
```

Observe in staging:

```bash
kubectl -n ignition-system logs deploy/ignition-prober          # one line per journey per cycle
kubectl -n ignition-system port-forward svc/ignition-prober 9102 &
curl -s localhost:9102/metrics | grep ignition_probe            # ignition_probe_up, *_journey_runs_total, ...
```

### Extending to prod later

Add a `prod` target (`requireApproval: true`) as a second stage in
`deploy/clouddeploy/pipeline.yaml`, add a `prod` profile to `skaffold.yaml` pointing at
`deploy/k8s/overlays/prod`, then `gcloud deploy releases promote`. A Cloud Deploy `Automation` can
auto-promote staging→prod after a soak while keeping the approval gate.

### Known gaps

- **Database schema rollout is not separated from API startup.** `ignition-api` currently applies
  the complete, idempotent `internal/store/schema.sql` baseline when it starts. Before schema
  evolution is needed, add a reviewed predeploy schema job and remove DDL privileges from the API.
- **Cloud Deploy `verify` runs only the read-only journeys** (`lite`, unauthenticated). Full lifecycle
  CUJ coverage on staging comes from the continuous `ignition-prober` Deployment and from
  `tests/integration` (`TestProbeJourneys`) in CI. To run the full set in `verify` too, give the
  skaffold verify Job the `ignition-prober` ServiceAccount, `IGNITION_PROBE_AUTH=gcp-idtoken`, and
  `IGNITION_PROBE_AUDIENCE` equal to the API's `IGNITION_OIDC_AUDIENCE`.
- **`tests/conformance/` is superseded** by `internal/probe` + `cmd/ignition-prober` and can be removed.
- **The controller has no health signal.** `cmd/ignition-controller` serves no HTTP, so its Deployment
  has no liveness/readiness probe, and `controller.Run` uses a non-cancellable `context.Background()`
  (SIGTERM is not graceful; lease expiry after `LeaseTTL` covers failover on a hard kill). Follow-up:
  add a `/healthz` listener + `signal.NotifyContext`, mirroring `internal/sandboxinit/init.go`.
- **Staging warm pool.** `overlays/staging/config.yaml` now sets `IGNITION_MIN_WARM: "0"` so the
  `gpu-sandbox-l4` pool scales to zero when idle. Raise it only while testing warm-pool behaviour.
