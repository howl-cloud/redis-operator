# config/manager

Kubernetes manifests for deploying the redis-operator controller-manager.

## Files

| File | Description |
|------|-------------|
| `manager.yaml` | `Deployment` for the operator, with leader election and webhook TLS mounts |
| `service.yaml` | `Service` exposing metrics (`:8080`) and webhook (`:9443`) ports |
| `kustomization.yaml` | Kustomize entry point |

## Operator Pod Template

The operator pod runs a single container using the `redis-operator` image with subcommand `controller`. It mounts:
- Webhook TLS certs from the self-managed `redis-operator-webhook-cert` Secret.
- A projected `ServiceAccount` token for Kubernetes API access.

Leader election uses a `Lease` object (`redis-operator-leader`) in the operator's namespace.
