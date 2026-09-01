# Ignition infrastructure as code

This directory provisions the Google Cloud resources used by the regional dev deployment. It replaces the infrastructure portion of the [`gcloud` runbook](../../docs/guides/ignition-implementation.md#deploy-regional-dev); Kubernetes manifests, image builds, database schema/grants, and GPU quota remain separate.

## What Terraform owns

- Enabled GCP APIs, custom VPC, secondary ranges, Cloud NAT, and Private Services Access.
- Private Google API DNS/route configuration and the guide's default-deny node egress firewall policy.
- Regional private GKE with Dataplane V2, Workload Identity, image streaming, a 1–3 node system pool, scale-to-zero restricted CPU/GPU gVisor pools, and matching scale-to-zero internet-enabled CPU/GPU pools. Internet pools use a separate network tag and Cloud NAT-backed egress policy.
- Regional-HA PostgreSQL 16 Cloud SQL with private IP, automated backups, seven-day PITR logs, deletion protection, the `ignition` database, and password-authenticated `ignition` user.
- An optional regional-HA cross-region Cloud SQL read replica when `dr_region` is set.
- Control-plane and sandbox Artifact Registry repositories.
- Dedicated node, API, controller, and CUJ-prober Google service accounts; Workload Identity bindings; repository-scoped image-pull access; and the IAM roles required by the runbook. The prober SA holds no project IAM role - it is authorized inside ignition-api by a `role_bindings` row (`db/rolebindings.sql`).
- Optional Cloud IAP access grants (`iap_enabled` + `iap_members`) for the ignition-api HTTPS load balancer. IAP itself is turned on per-backend by `deploy/k8s/components/iap` and uses Google-managed OAuth, so there is no OAuth client to create; Terraform only enables `iap.googleapis.com` and grants `roles/iap.httpsResourceAccessor` at the project compute-web scope (the GKE Ingress names the backend service dynamically).

Terraform deliberately does not create Kubernetes objects. Apply the appropriate Kustomize overlay after infrastructure is ready. Do not run the old `gcloud` provisioning commands against the same resources after Terraform takes ownership.

## Prerequisites

Install Terraform 1.6+ and the Google Cloud CLI. A user-local Linux installation can place the verified HashiCorp binary at `~/.local/bin/terraform`; ensure that directory is on `PATH`, then confirm with `terraform version`. Authenticate with an account that can create the resources, select a billed project, and ensure regional GKE/L4 quota is available. The provider is pinned to Google provider versions `>= 6.0, < 8.0`.

## New environment

```bash
cd deploy/terraform
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars: project_id and operator_cidr at minimum.
export TF_VAR_sql_password='use-a-secret-value'
terraform init
terraform fmt -check
terraform validate
terraform plan -out=tfplan
terraform apply tfplan
```

Run formatting and validation after every configuration change. `terraform validate` checks configuration and provider schemas without creating or changing cloud resources; review `terraform plan` separately before any apply.

Keep `terraform.tfvars`, `tfplan`, and `.terraform/` out of source control. For a shared environment, configure a versioned, access-controlled GCS backend before `terraform init` and use one state prefix per environment.

Do not put `sql_password` in `terraform.tfvars`; pass it through `TF_VAR_sql_password` or the deployment system's secret injection. Terraform state still contains the managed SQL user password, so the state backend must be encrypted and access-controlled. Set `dr_region` only when the additional cross-region database cost and a tested promotion procedure are intended.

After apply:

```bash
gcloud container clusters get-credentials "$(terraform output -raw cluster_name)" \
  --region "$(terraform output -raw cluster_region)"
```

Then follow the implementation guide for database grants, the Kubernetes secret, image push, and the rendered dev overlay. Terraform already created the `ignition` SQL user, so carry the same password into the bootstrap step and suppress duplicate user creation:

```bash
export SQL_PASS="${TF_VAR_sql_password:?TF_VAR_sql_password must still contain the applied value}"
export SQL_USER_ALREADY_EXISTS=true
```

Useful outputs include the cluster name and region, registry paths, Cloud SQL connection name/private IP, and Google service-account emails (`api_service_account`, `controller_service_account`, `node_service_account`, `prober_service_account`). Use the service-account outputs when rendering the Workload Identity annotations and `db/rolebindings.sql`; do not commit a project-specific email to the reusable Kustomize base.

For staging/prod, set `iap_enabled = true` and `iap_members` (e.g. `["group:eng@your-domain"]`), apply, then include `deploy/k8s/components/iap` in the overlay after filling `IGNITION_IAP_AUDIENCE` (the backend-service resource path, obtainable only after the Ingress first syncs).

## Migrating an existing gcloud environment

Use a backup and a dedicated state file. Import every existing resource before applying; never let Terraform recreate a cluster, node pool, peering connection, or SQL instance during migration. A minimal start is:

```bash
terraform import google_compute_network.main projects/PROJECT/global/networks/ignition-vpc
terraform import google_compute_subnetwork.main projects/PROJECT/regions/us-central1/subnetworks/ignition-subnet
terraform import google_compute_router.main projects/PROJECT/regions/us-central1/routers/ignition-router
terraform import google_compute_router_nat.main projects/PROJECT/regions/us-central1/routers/ignition-router/ignition-nat
terraform import google_container_cluster.main projects/PROJECT/locations/us-central1/clusters/ignition
terraform import google_container_node_pool.system projects/PROJECT/locations/us-central1/clusters/ignition/nodePools/cpu-system
terraform import google_container_node_pool.cpu_sandbox projects/PROJECT/locations/us-central1/clusters/ignition/nodePools/cpu-sandbox
terraform import google_container_node_pool.gpu_sandbox projects/PROJECT/locations/us-central1/clusters/ignition/nodePools/gpu-sandbox-l4
terraform import google_sql_database_instance.main PROJECT:ignition-sql
terraform import google_artifact_registry_repository.control_plane projects/PROJECT/locations/us-central1/repositories/ignition
```

The configuration uses explicit `cpu-system`, `cpu-sandbox`, and `gpu-sandbox-l4` pools. If the existing cluster still has the gcloud-managed default pool, reconcile that pool in a reviewable plan (or migrate into a fresh cluster) before applying. Import the remaining database, IAM, API, PSA, and repository resources as appropriate for the project, then require a no-recreate plan review.

## Destruction

`terraform destroy` removes billable infrastructure, including the cluster and database. SQL deletion protection defaults to on. An intentional SQL deletion therefore requires a reviewed two-step change: set `sql_deletion_protection = false`, apply it, and only then destroy. Take and verify a database backup first.

Regional HA protects against a zonal failure, not a regional outage. Terraform does not create a cross-region replica by default because it adds another billable database and requires an environment-specific DR region and promotion plan. Set `dr_region` to create it when that control is required; otherwise record explicit acceptance of the multi-region finding for a disposable dev environment. Follow the implementation guide's DR section to monitor lag and test promotion.
