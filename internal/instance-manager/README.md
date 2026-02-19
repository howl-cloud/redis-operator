# internal/instance-manager

The in-pod agent that runs as PID 1 inside every Redis pod.

## Overview

The instance manager is started via `redis-operator instance`. It:

1. Initializes Redis data directory (fresh start or restore from backup).
2. Writes `redis.conf` from the cluster ConfigMap and pod-specific parameters.
3. Starts `redis-server` as a supervised child process.
4. Runs an HTTP server for Kubernetes probes and operator commands.
5. Runs an in-pod reconcile loop that watches the `RedisCluster` CR via the Kubernetes API.

The instance manager has minimal RBAC: read-only on its own `RedisCluster`, read on relevant `ConfigMap`/`Secret` objects, and patch on `RedisCluster/status` to report its own state.

## Package Layout

| Package | Description |
|---------|-------------|
| `run/` | Top-level `Run()` function: initializes, starts redis-server, blocks until exit |
| `reconciler/` | `InstanceReconciler`: watches `RedisCluster`, reconciles config and role |
| `webserver/` | HTTP server for liveness/readiness probes and operator commands |
| `replication/` | Helpers to configure/query Redis replication (`REPLICAOF`, `INFO replication`) |

## Communication with the Operator

Two channels:

**Kubernetes API (primary)**
- Watches `RedisCluster` for spec changes (config updates, fencing, target primary).
- Patches `RedisCluster.status.instancesStatus[<podName>]` with live Redis metrics.

**HTTP API (secondary — for instance-specific commands)**
- The operator calls the pod IP directly (not through a Service).
- Endpoints: see [`webserver/README.md`](webserver/README.md).

## Fencing

If `RedisCluster` carries a fence annotation for this pod's name, the reconciler stops `redis-server` immediately and refuses to restart it until the annotation is cleared. Used for maintenance and split-brain prevention.

## In-place Binary Upgrade

The operator can upgrade the instance manager binary without restarting `redis-server`:
1. Operator writes the new binary to a pod volume via the init container on next pod spec hash change — or triggers `PUT /v1/update` with the new binary.
2. The instance manager calls `syscall.Exec` to replace itself with the new binary.
3. `redis-server` continues running uninterrupted as the child process is inherited by the new PID.
