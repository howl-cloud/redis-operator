# AGENTS.md

Agent guidance for the `redis-operator` codebase. Read this before making changes.

## What this is

A Kubernetes operator for Redis 7.2 clusters (wire-compatible with Valkey 7.x/8.x). CNPG-inspired: direct Pod/PVC management, no StatefulSets. Two roles share one OCI image — a controller-manager and an in-pod instance manager.

---

## Before you code

**Use the golang-pro skill.** Invoke `/golang-pro` before writing or reviewing non-trivial Go. It enforces idioms that matter here.

**Hard invariants — never break these:**

- `context.Context` is always the first argument on any function that does I/O, network calls, or Kubernetes API calls.
- Errors are returned, not panicked. Use `errors.Is`/`errors.As` — never string-match error messages.
- Operator-to-pod communication always uses the **pod IP directly**, never a Service. Services are load-balanced; we need to target a specific pod for `/v1/promote`, `/v1/backup`, etc.
- The split-brain guard in `internal/instance-manager/run/run.go` must fire before `redis-server` starts — if `POD_NAME != status.currentPrimary`, always issue `REPLICAOF` first, regardless of local data.
- Fencing annotation goes on **before** promoting a replica. Never promote without fencing first.
- Status is updated via `status` subresource only (separate from spec). Per-pod state lives in a map keyed by pod name, never a slice (avoids strategic-merge-patch ordering bugs).
- Rolling updates: replicas always before the primary (highest ordinal first). The primary is last, promoted out via switchover, not killed directly.
- Secrets are injected as projected volumes, never env vars.
- `cluster` mode (`spec.mode: cluster`) is reserved and intentionally not implemented. The webhook rejects it. Don't implement it without a dedicated issue.

**Run codegen after editing API types.** Any change to `api/v1/*_types.go` requires:
```bash
make generate   # regenerates zz_generated.deepcopy.go
make manifests  # regenerates config/crd/bases/
```

**Run `make lint` before finishing.** CI will catch it anyway; fix it locally first.

---

## Commands

```bash
# Codegen (required after api/v1/ edits)
make generate        # controller-gen → zz_generated.deepcopy.go
make manifests       # controller-gen → config/crd/bases/

# Build & lint
make build           # go build ./...
make lint            # golangci-lint run

# Test
make test            # unit tests (no cluster needed)
make test-integration   # real Redis via testcontainers (Docker required)
make test-e2e           # envtest (local kube-apiserver + etcd)
make test-smoke-kind    # full lifecycle on a kind cluster
make test-chaos-kind    # fault injection on a kind cluster

# Targeted test runs
go test ./internal/controller/cluster/...
go test -run TestReconcileFailover ./...
go test -bench=. ./internal/controller/cluster/...   # benchmarks

# Local development
make run             # operator against current kubeconfig
make install         # kubectl apply CRDs
make deploy          # kustomize build | kubectl apply
```

---

## Key files by area

| Area | Files |
|---|---|
| Binary entry point | `cmd/manager/main.go` — three subcommands: `controller`, `instance`, `copy-binary` |
| CRD types | `api/v1/rediscluster_types.go`, `redisbackup_types.go`, `redisscheduledbackup_types.go` |
| Cluster reconciler | `internal/controller/cluster/reconciler.go` |
| Pod/PVC lifecycle | `internal/controller/cluster/pods.go`, `pvcs.go` |
| Failover & fencing | `internal/controller/cluster/fencing.go`, `rolling_update.go` |
| Sentinel mode | `internal/controller/cluster/sentinel.go`, `internal/instance-manager/run/sentinel.go` |
| Secrets | `internal/controller/cluster/secrets.go` |
| Instance manager startup | `internal/instance-manager/run/run.go` |
| Instance HTTP API | `internal/instance-manager/webserver/server.go` — `/v1/status`, `/v1/promote`, `/v1/backup` |
| Instance reconciler | `internal/instance-manager/reconciler/reconciler.go` |
| Webhook PKI | `internal/cmd/manager/controller/pki.go`, `pki_reconciler.go` |
| Webhooks | `webhooks/rediscluster_defaulter.go`, `rediscluster_validator.go` |
| Backup | `internal/controller/backup/reconciler.go`, `executor.go`, `scheduled_reconciler.go` |
| RBAC | `config/rbac/role.yaml` — source of truth for all permissions |
| Helm chart | `charts/redis-operator/` |
| Runbooks | `docs/runbooks/` — failover, split-brain, PVC corruption, secret rotation, etc. |

---

## How to work in each area

### Adding or changing API types (`api/v1/`)

1. Edit the `*_types.go` file.
2. Use `// +kubebuilder:validation:` markers for all constraints — don't rely on webhook validation alone.
3. New status fields go in the `Status` struct with `// +optional`; document the phase transitions.
4. Run `make generate && make manifests` — commit both the type change and the generated files together.
5. If adding a new field that needs a default, add it in `webhooks/rediscluster_defaulter.go`.

### Adding controller logic (`internal/controller/cluster/`)

- Add sub-steps to the `reconcile()` sequence in `reconciler.go` following the existing order. Don't reorder existing steps.
- Return `ctrl.Result{RequeueAfter: ...}` for expected-transient states; return an error only for unexpected failures.
- Record events (`r.recorder.Event(...)`) for anything a human might want to see in `kubectl describe`.
- Test with a `*_test.go` file in the same package using `envtest` + Ginkgo/Gomega. See `reconciler_test.go` for patterns.

### Changing instance manager behavior (`internal/instance-manager/`)

- The HTTP endpoints in `webserver/server.go` are the operator→pod contract. Don't remove or rename them without updating all callers in `internal/controller/cluster/status.go` and `fencing.go`.
- Live config changes (ACL, password) go through `reconciler/reconciler.go` via `CONFIG SET` / `ACL LOAD` — not pod restarts.
- Integration tests in `test/integration/` use real Redis via testcontainers. Run them with `make test-integration`.

### Modifying RBAC

- `config/rbac/role.yaml` is the **only** source of truth. If the controller needs a new API resource, add it here.
- For OLM bundle work, the ClusterServiceVersion CSV must match this file exactly — generate it from here, don't hand-edit the CSV.
- After RBAC changes, run `make manifests` to propagate.

### Writing tests

Follow existing patterns:
- **Unit tests**: table-driven, `*_test.go` alongside the code. Use `testify` or standard `testing` only — not Ginkgo.
- **Controller tests**: Ginkgo/Gomega + `envtest`. See `test/e2e/` for suite setup.
- **Integration tests**: testcontainers-go for real Redis. Docker must be running.
- Always clean up resources in `AfterEach` / `DeferCleanup`. The existing cleanup patterns in `test/e2e/` are hardened — follow them.
- Don't mock Kubernetes clients in controller tests — use `envtest`. Don't mock Redis in integration tests — use testcontainers.

---

## Reconciliation order (don't reorder)

`reconcile()` in `internal/controller/cluster/reconciler.go`:

1. Global resources — ServiceAccount, RBAC, ConfigMap, PDB
2. Secret resolution — resolves spec secret refs, updates `status.secretsResourceVersion`
3. Services — `-leader`, `-replica`, `-any`
4. HTTP status poll — `GET /v1/status` on every live pod IP
5. Status update
6. Reachability check — requeue if any expected pod unreachable
7. PVC reconciliation
8. Pod reconciliation — scale up/down, rolling updates

---

## CRD quick reference

| Kind | Modes | Notes |
|---|---|---|
| `RedisCluster` | `standalone`, `sentinel` | `cluster` mode reserved/unimplemented |
| `RedisBackup` | — | On-demand RDB/AOF to object storage |
| `RedisScheduledBackup` | — | Cron-based; delegates to `RedisBackup` |

Phase values: `Creating`, `Healthy`, `Degraded`, `FailingOver`, `Scaling`, `Updating`, `Deleting`, `Hibernating`.

---

## Things that are intentionally absent

- **No cert-manager** — webhook PKI is self-managed in `internal/cmd/manager/controller/pki.go`.
- **No StatefulSets** — Pods and PVCs are managed directly for full lifecycle control.
- **No env-var secret injection** — projected volumes only.
- **No cluster mode** — not yet; webhook rejects it.
