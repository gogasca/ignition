# Terraform (GCP infrastructure)

`gcloud` in the [implementation guide](../../docs/guides/ignition-implementation.md) is the **bootstrap**. Terraform should replace those create commands after one environment is proven — not in parallel.

## Can `gcloud` be replaced?

**Yes** for project-level GCP: VPC, GKE (CPU + L4 sandbox pool), Cloud SQL, Artifact Registry, IAM/Workload Identity GSAs, global IPs.

**Keep Kustomize** for in-cluster objects (Deployments, RBAC, Ingress). Image tags change every SHA; overlays are the right tool. A Terraform Kubernetes provider is optional later and is not required to retire `gcloud`.

**Never both.** If Terraform and `gcloud` both “own” the GPU node pool, you will get drift or a second pool. After import, delete the gcloud runbook from the operator path (leave it as archaeology in the guide).

## Suggested layout (when you write `.tf`)

One root module, one workspace (or state prefix) for `ignition-dev`. Variables: `project`, `gpu_max_nodes`, `sql_tier`, `sql_availability`.

| `gcloud` today | Terraform resource |
|---|---|
| `services enable` | `google_project_service` |
| `compute networks/subnets/routers/nats` | `google_compute_network`, `google_compute_subnetwork`, `google_compute_router`, `google_compute_router_nat` |
| `compute addresses` (PSA + Ingress) | `google_compute_global_address` |
| `services vpc-peerings connect` | `google_service_networking_connection` |
| `container clusters create` | `google_container_cluster` (`remove_default_node_pool = true` is fine; we use the default CPU pool today) |
| `container node-pools create gpu-sandbox-l4` | `google_container_node_pool` + **google-beta** `sandbox_config { sandbox_type = "gvisor" }`, taint, `nvidia-l4`, `gvnic` as required by GKE |
| `sql instances/databases/users` | `google_sql_database_instance`, `google_sql_database`, `google_sql_user` (type `CLOUD_IAM_SERVICE_ACCOUNT`) |
| `artifacts repositories` | `google_artifact_registry_repository` |
| `iam service-accounts` + bindings | `google_service_account`, `google_service_account_iam_member`, `google_project_iam_member` |

GKE Sandbox on the node pool is the piece that most often needs **google-beta**. Pin provider versions. State in a GCS bucket with object versioning, one bucket prefix per env.

## Import, do not recreate

After the gcloud cluster exists:

```bash
terraform import google_container_cluster.ignition projects/PROJECT/locations/us-central1/clusters/ignition
terraform import google_container_node_pool.gpu_sandbox projects/PROJECT/locations/us-central1/clusters/ignition/nodePools/gpu-sandbox-l4
```

`terraform plan` must be empty (or only tags) before you apply. Recreating the L4 pool destroys tenant isolation and quota burn.

## Out of scope for Terraform

- NVIDIA L4 quota increases (console)
- DNS at a third-party registrar (optional `google_dns_record_set` if Cloud DNS)
- Auth0/OIDC application
- `kubectl apply -k deploy/k8s/overlays/*`
- Customer sandbox image builds

## Status

No `.tf` files yet. Write them when `gcloud` has brought up **dev** once without hand-edits to the cluster. Until then the copy-paste in the [implementation guide](../../docs/guides/ignition-implementation.md#deploy-regional-dev) is the source of truth for flags (`g2-standard-8`, one L4, `gvisor`, taint, private nodes).
