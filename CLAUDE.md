# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

A Kubernetes operator for Redis 7.2 clusters, modelled after [CloudNative-PG](https://github.com/cloudnative-pg/cloudnative-pg). Targets Redis 7.2 (BSD-licensed); wire-compatible with Valkey 7.x/8.x as a drop-in.

## Go Development

This is a Go codebase. Use the `golang-pro` Claude skill when writing or reviewing Go code in this repository — invoke it with `/golang-pro`.

Follow standard Go best practices: keep interfaces small and defined at the point of use, prefer `errors.Is`/`errors.As` over string matching, use `context.Context` as the first argument on all functions that do I/O, and return errors rather than panicking. Concurrency in the instance manager (goroutines for the HTTP server, reconciler, and child process supervision) must be coordinated via `context.Context` cancellation — not global state or bare `sync.WaitGroup` without a context.

## Commands

```bash
# Generate DeepCopy methods and CRD manifests (run after editing api/v1/*_types.go)
make generate   # controller-gen → zz_generated.deepcopy.go
make manifests  # controller-gen → config/crd/bases/

# Build
make build      # go build ./...

# Lint
make lint       # golangci-lint run

# Test
make test                                    # all unit tests
go test ./internal/controller/cluster/...   # single package
go test -run TestReconcileFailover ./...     # single test by name

# Run operator locally against current kubeconfig cluster
make run        # go run ./cmd/manager controller

# Install CRDs into the cluster
make install    # kubectl apply -f config/crd/bases/

# Deploy operator to cluster
make deploy     # kustomize build config/ | kubectl apply -f -
```

## Architecture

### Two-binary, one image

`cmd/manager/main.go` produces a **single binary** with two Cobra subcommands:

- `redis-operator controller` — the Kubernetes controller-manager (`Deployment` in the cluster)
- `redis-operator instance` — the in-pod agent (PID 1 inside every Redis pod)

The same OCI image is used for both. An init container copies the binary into a shared `emptyDir` volume (`/controller/`), and the Redis pod's `command` is set to `/controller/manager instance`.

### No StatefulSets

`internal/controller/cluster/` manages `Pod` and `PVC` objects directly. This gives full control over creation order, deletion policy, and rolling updates. Each pod's PVC survives pod deletion and is reattached on recreate.

### Reconciliation order (`ClusterReconciler`)

`reconcile()` in `internal/controller/cluster/reconciler.go` runs sub-steps in this fixed order:
1. Global resources (ServiceAccount, RBAC, ConfigMap, PodDisruptionBudget)
2. Secret resolution — resolves all `ClusterSpec` secret refs, updates `status.secretsResourceVersion`
3. Services (`-leader`, `-replica`, `-any`)
4. HTTP status poll — calls `GET /v1/status` on every live pod IP
5. Status update
6. Reachability check — requeue if any expected pod is unreachable
7. PVC reconciliation
8. Pod reconciliation (scale up/down, rolling updates)

### Instance manager

`internal/instance-manager/` runs inside every Redis pod. On startup it:
1. Fetches `RedisCluster` from the API
2. **Split-brain guard**: if `POD_NAME != status.currentPrimary`, unconditionally writes `replicaof` before starting `redis-server` (regardless of local data)
3. Spawns `redis-server` as a child process

It then runs two goroutines: an HTTP server (`:8080`) and an `InstanceReconciler` that watches the `RedisCluster` CR. The instance manager patches `status.instancesStatus[podName]` and applies live config changes without pod restart (`CONFIG SET requirepass`, `ACL LOAD`).

### Failover sequence (split-brain safe)

1. Operator detects primary unreachable via HTTP poll timeout
2. **Fence the former primary first** (annotation on `RedisCluster`) — prevents it from accepting writes if it recovers mid-failover
3. Select lowest-lag replica from `status.instancesStatus`
4. `POST /v1/promote` → instance manager runs `REPLICAOF NO ONE`
5. Update `-leader` Service selector + `status.currentPrimary`
6. Remove fence — former primary restarts and the split-brain guard (step 2 of startup) forces it to rejoin as a replica

### Secret resolution

`internal/controller/cluster/secrets.go` resolves `authSecret`, `aclConfigSecret`, `tlsSecret`, `caSecret`, `backupCredentialsSecret` from the spec. Secrets are injected as **projected volumes** (not env vars). `status.secretsResourceVersion` tracks each secret's `ResourceVersion`; a change re-enqueues the cluster and the instance manager applies it live.

### CRDs — `api/v1/`

| Kind | Purpose |
|---|---|
| `RedisCluster` | Core: instances, mode (`standalone`/`sentinel`/`cluster`), storage, replication quorum, secret refs, topology |
| `RedisBackup` | On-demand RDB/AOF backup to object storage |
| `RedisScheduledBackup` | Cron-based scheduled backups |

All types use `// +kubebuilder:subresource:status` so spec and status have independent update paths. Status uses `[]metav1.Condition` alongside a human-readable `Phase` string. Per-pod state is stored as a map (not a slice) keyed by pod name to avoid strategic-merge-patch ordering issues.

### Key design constraints

- **Operator calls pod IPs directly** — never through a Service — for instance-specific operations (`/v1/promote`, `/v1/backup`). Services are load-balanced and can't target a specific pod.
- **Rolling updates**: replicas updated first (highest ordinal), primary last via switchover.
- **PDB**: `minAvailable = max(1, instances-1)`, managed by `pdb.go`, controlled by `spec.enablePodDisruptionBudget` (default `true`).
- **Webhook PKI is self-managed** — no cert-manager dependency. CA and webhook cert are rotated by a controller loop in `internal/cmd/manager/controller/pki.go`.
- **`/metrics`** on `:9090` (separate port from the operator HTTP API on `:8080`) exposes Prometheus metrics sourced from `INFO all`.
