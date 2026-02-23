# Contributing to redis-operator

This project aims to provide a production-grade Redis operator using CNPG-style control-plane patterns. Contributions are welcome for code, tests, docs, and operational hardening.

## Architecture Expectations

Before changing behavior, align with the core design:

- Direct pod/PVC orchestration by the operator (no StatefulSet abstraction)
- Single binary with two runtime roles (`controller` and in-pod `instance`)
- Controller remains source of truth via CRD spec/status
- Safety-first failover and rolling-update sequencing

If a change intentionally departs from this model, explain the trade-off clearly in the PR.

## Developer Quick Start

### Prerequisites

- Go `1.25+` (see `go.mod`)
- Docker
- Kubernetes tooling: `kubectl`, `helm`
- `kind` (for smoke/chaos tests)

### Validate your environment quickly

```bash
make test
make lint
```

### Run integration tests (real Redis via Docker)

```bash
make test-integration
```

### Run envtest e2e suite

```bash
make setup-envtest
make test-e2e
```

### Run full kind smoke test (recommended before opening a PR)

```bash
make test-smoke-kind
```

This test builds the image, deploys the operator via Helm to a kind cluster, creates a `RedisCluster`, and validates lifecycle operations.

## Local Development Loop (Manual Cluster)

If you want a persistent local cluster while iterating:

```bash
kind create cluster --name redis-operator-dev
docker build -t redis-operator:dev .
kind load docker-image redis-operator:dev --name redis-operator-dev
kubectl apply -f config/crd/bases/
helm upgrade --install redis-operator charts/redis-operator \
  --namespace redis-operator-system \
  --create-namespace \
  --set image.repository=redis-operator \
  --set image.tag=dev \
  --set image.pullPolicy=IfNotPresent
```

Create a test cluster:

```yaml
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: dev-redis
  namespace: default
spec:
  instances: 3
  mode: standalone
  storage:
    size: 1Gi
```

```bash
kubectl apply -f rediscluster.yaml
kubectl get rediscluster dev-redis -n default -w
```

## Project Map

- `api/v1/`: CRD API types
- `internal/controller/cluster/`: core `RedisCluster` reconciliation
- `internal/controller/backup/`: backup and scheduled backup controllers
- `internal/instance-manager/`: in-pod agent (redis process supervision + reconcile)
- `webhooks/`: defaulting and validating admission logic
- `charts/redis-operator/`: Helm chart
- `docs/runbooks/`: operations and incident response playbooks

## Pull Request Guidelines

- Keep changes scoped and explain user-visible behavior changes
- Add or update tests for non-trivial logic changes
- Run relevant checks before requesting review:
  - `make test`
  - `make lint`
  - `make test-integration` (if Redis/runtime behavior changed)
  - `make test-smoke-kind` (if controller/pod lifecycle behavior changed)
- Update docs when API, behavior, or operator workflows change

For larger changes (new CRD fields, reconciliation strategy changes, failover semantics), include a short design note in the PR description.
