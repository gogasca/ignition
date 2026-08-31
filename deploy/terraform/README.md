# Terraform (GCP infrastructure)

There is no Terraform implementation in this repository yet. The [`gcloud` implementation guide](../../docs/guides/ignition-implementation.md#deploy-regional-dev) is the current source of truth for dev infrastructure. Introduce Terraform only by importing or replacing those resources deliberately; do not let both workflows own the same environment.

## Current resource boundary

The current dev runbook creates:

- required Google APIs;
- a custom VPC, subnet, secondary Pod/Service ranges, router, NAT, and private-services access range;
- a regional GKE Standard cluster using Dataplane V2, with a CPU pool and a zero-to-two-node L4/gVisor pool;
- a private-IP, zonal PostgreSQL 16 Cloud SQL instance, database, and password application user;
- separate Artifact Registry repositories for control-plane and sandbox images; and
- `ignition-api` and `ignition-controller` Google service accounts plus Workload Identity and project IAM bindings.

Keep Kustomize responsible for in-cluster resources. The dev overlay deploys the API, controller, RBAC, and Cloud SQL Auth Proxy sidecars. Sandbox Pods are created dynamically by the controller. Staging/prod Ingress manifests exist as templates but are not part of the validated dev runbook.

## Suggested Terraform mapping

Use one root module and one isolated state prefix or workspace per environment. At minimum, expose the project, region/zone, network ranges, GPU maximum, SQL tier, and deletion-protection choices as variables.

| `gcloud` today | Terraform resource |
|---|---|
| `services enable` | `google_project_service` |
| `compute networks/subnets/routers/nats` | `google_compute_network`, `google_compute_subnetwork`, `google_compute_router`, `google_compute_router_nat` |
| `compute addresses` (private-services access) | `google_compute_global_address` |
| `services vpc-peerings connect` | `google_service_networking_connection` |
| `container clusters create` | `google_container_cluster` with private nodes, Workload Identity, Dataplane V2, and the regional CPU pool behavior from the guide |
| `container node-pools create gpu-sandbox-l4` | `google_container_node_pool` with `g2-standard-8`, one `nvidia-l4`, gVisor sandbox config, taint/label, and total autoscaling bounds |
| `sql instances/databases/users` | `google_sql_database_instance`, `google_sql_database`, `google_sql_user` for the password-authenticated `ignition` user |
| `artifacts repositories` | `google_artifact_registry_repository` |
| `iam service-accounts` + bindings | `google_service_account`, `google_service_account_iam_member`, `google_project_iam_member` |

Pin provider versions and keep state in a versioned, access-controlled GCS bucket. Confirm the chosen provider version supports the GKE sandbox and total autoscaling fields used by the runbook before implementation.

## Import, do not recreate

After the gcloud cluster exists:

```bash
terraform import google_container_cluster.ignition projects/PROJECT/locations/us-central1/clusters/ignition
terraform import google_container_node_pool.gpu_sandbox projects/PROJECT/locations/us-central1/clusters/ignition/nodePools/gpu-sandbox-l4
```

Import every resource that Terraform will own, then reconcile configuration until `terraform plan` contains only intentional changes. Do not apply a plan that recreates the cluster, GPU pool, private-services connection, or Cloud SQL instance merely to complete the migration.

## Out of scope for Terraform

- NVIDIA L4 quota increases (console)
- external DNS and OIDC-provider configuration
- `kubectl apply -k deploy/k8s/overlays/*`
- control-plane and sandbox image builds

## Status

No `.tf` files exist. The copy/paste commands in the [implementation guide](../../docs/guides/ignition-implementation.md#deploy-regional-dev) remain authoritative for the current `g2-standard-8`/L4/gVisor, private-network, Dataplane V2, Cloud SQL, registry, and IAM configuration.
