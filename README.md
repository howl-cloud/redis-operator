# redis-operator

A Kubernetes operator for managing Redis 7.2 clusters, modelled after the architecture of [CloudNative-PG](https://github.com/cloudnative-pg/cloudnative-pg).

## Overview

redis-operator manages the full lifecycle of Redis instances on Kubernetes: bootstrapping, replication, failover, rolling updates, backups, and configuration management — all expressed as Kubernetes-native custom resources.

Key design decisions inherited from CloudNative-PG:

- **No StatefulSets.** Each Redis pod and its PVC are managed directly by the operator, giving full control over creation order, deletion policy, and rolling updates.
- **Two-binary model.** A single compiled binary serves as both the operator controller-manager (run as a `Deployment`) and the in-pod instance manager (run as PID 1 inside each Redis pod), selected via CLI subcommand.
- **Instance manager as PID 1.** The instance manager binary supervises the `redis-server` process, watches the Kubernetes API for cluster changes, reconciles local Redis configuration, and exposes an HTTP API for liveness/readiness probes and operator-to-pod commands.
- **Operator polls instance managers over HTTP.** For status collection and instance-specific operations (promote, backup), the operator calls each pod's IP directly — bypassing the load-balanced Service layer.
- **Three-service pattern.** The operator maintains three Services per cluster: `-leader` (read/write, points to primary), `-replica` (read-only, load-balanced across replicas), and `-any` (any instance).

## Architecture

```
┌───────────────────────────────────────────────────────────┐
│                  Kubernetes Cluster                        │
│                                                           │
│  ┌─────────────────────────────────────────────────────┐  │
│  │     redis-operator Deployment (controller-manager)  │  │
│  │                                                     │  │
│  │  ClusterReconciler  BackupReconciler  Webhooks      │  │
│  └────────────┬────────────────────────────────────────┘  │
│               │ watch/update                               │
│               ▼                                            │
│  ┌─────────────────────────────────────────────────────┐  │
│  │  RedisCluster CR  (desired state)                   │  │
│  └─────────────────────────────────────────────────────┘  │
│               │ creates/manages                            │
│    ┌──────────┼───────────────┐                           │
│    ▼          ▼               ▼                           │
│  Pod-0      Pod-1           Pod-2                         │
│  PVC-0      PVC-1           PVC-2                         │
│  (primary)  (replica)       (replica)                     │
│                                                           │
│  Each pod runs: instance-manager (PID 1) → redis-server  │
│                                                           │
│  Services:  <name>-leader  <name>-replica  <name>-any    │
└───────────────────────────────────────────────────────────┘
```

## Custom Resources

| CRD | Description |
|-----|-------------|
| `RedisCluster` | Defines a Redis replication cluster (primary + N replicas) |
| `RedisBackup` | On-demand RDB/AOF backup to object storage |
| `RedisScheduledBackup` | Cron-based scheduled backups |

## Project Layout

```
cmd/manager/              Binary entry point (Cobra CLI, subcommands: controller / instance)
api/v1/                   CRD type definitions (RedisCluster, RedisBackup, ...)
internal/
  cmd/manager/controller/ RunController: wires ctrl.Manager, registers reconcilers + webhooks
  controller/
    cluster/              ClusterReconciler — pods, PVCs, services, rolling updates, status
    backup/               BackupReconciler — on-demand and scheduled backup execution
  instance-manager/       In-pod agent: supervises redis-server, HTTP API, reconciles config
config/
  crd/                    Generated CRD YAML manifests (controller-gen output)
  rbac/                   ClusterRole, RoleBinding, ServiceAccount manifests
  manager/                Operator Deployment and related manifests
  webhook/                ValidatingWebhookConfiguration, MutatingWebhookConfiguration
webhooks/                 Defaulter and validator implementations
```

## Failover & Replication Model

- **Topology**: one primary, N-1 replicas using native Redis streaming replication (`REPLICAOF`).
- **Failover**: operator detects primary unavailability by polling instance manager HTTP endpoints. It selects the replica with the lowest replication lag, issues `REPLICAOF NO ONE` via the replica's HTTP endpoint, and updates the `-leader` Service selector.
- **Rolling updates**: replicas are updated one at a time (highest ordinal first). The primary is updated last, preceded by an automatic switchover.
- **Fencing**: a `RedisCluster` annotation can fence any instance, causing the instance manager to stop `redis-server` and refuse restart until the fence is lifted.

## Redis Version

Targets **Redis 7.2** (BSD-licensed). The operator is wire-protocol compatible with [Valkey](https://github.com/valkey-io/valkey) 7.x/8.x as a drop-in replacement.

## Development

See [`cmd/manager/README.md`](cmd/manager/README.md) to get started.
