# HoWL vs OpsTree Redis Operator

This document explains, in plain language, where the HoWL Redis Operator is stronger than `OT-CONTAINER-KIT/redis-operator` (OpsTree/OT), based on current code and DeepWiki context.

## Scope

- HoWL reference: `howl-cloud/redis-operator` at `6a2367d`
- OT reference: `OT-CONTAINER-KIT/redis-operator` at `7ae2c18`

## TL;DR (Layman's Version)

If you want Redis operations that are more strict, safety-first, and predictable during failures and upgrades, HoWL is the better fit.

In short:

- HoWL is built to prevent "two primaries" and bad failovers.
- HoWL gives you tighter control over how and when the primary is updated.
- HoWL targets exact pods directly for critical actions.
- HoWL keeps secrets out of env vars.

OT is more Kubernetes-default and StatefulSet-centric, and it has broad topology support and mature monitoring integration.

## Where HoWL Is Better

## 1) Safer Failover Flow

HoWL fences the old primary first, then promotes a replica. This is a deliberate safety sequence that reduces split-brain risk.

- HoWL:
  - `internal/controller/cluster/fencing.go` (fencing-first sequence)
  - `internal/controller/cluster/fencing.go` (`promoteInstance` targets a specific pod IP)
- OT:
  - Uses Sentinel-driven failover orchestration, but no equivalent explicit fencing-first contract was found in controller flow.

Why this matters in plain terms: HoWL puts a "stop writing" sign on the old leader before choosing a new one.

## 2) Split-Brain Defense at Startup and Runtime

HoWL has two protections:

- Boot-time guard: if a restarting pod is not the current primary, it is forced to start as replica (`REPLICAOF`).
- Runtime guard: a primary can fail health checks if it is isolated from both API server and peers, so Kubernetes replaces it.

- HoWL:
  - `internal/instance-manager/run/run.go` (boot-time role guard)
  - `internal/instance-manager/webserver/server.go` (`/healthz` primary-isolation checks)
- OT:
  - Health probes are primarily `redis-cli ping` style checks in StatefulSet configuration code.

Why this matters: HoWL is more aggressive about avoiding "isolated primary keeps accepting writes" scenarios.

## 3) More Predictable Primary Upgrades

HoWL updates replicas first and primary last. It also supports a supervised mode that pauses before touching primary and waits for explicit human approval.

- HoWL:
  - `api/v1/rediscluster_types.go` (`primaryUpdateStrategy`, `WaitingForUser`, approval annotation)
  - `internal/controller/cluster/rolling_update.go` (replica-first and supervised gate)
- OT:
  - Relies on StatefulSet update strategy (`partition`, rolling settings), not an equivalent operator-level primary approval gate.

Why this matters: primary change is the riskiest step, and HoWL lets you gate it on purpose.

## 4) Pod-Precise Control Plane

HoWL calls per-pod endpoints (`/v1/status`, `/v1/promote`, `/v1/backup`) over direct pod IP for critical operations.

- HoWL:
  - `internal/controller/cluster/status.go`
  - `internal/controller/cluster/fencing.go`
  - `internal/instance-manager/webserver/server.go`
- OT:
  - Controller + StatefulSet + Redis/Sentinel command model, without equivalent in-pod instance-manager API endpoints.

Why this matters: HoWL can reliably act on the exact pod you intended.

## 5) Stronger Secret Hygiene by Default

HoWL mounts auth/ACL/TLS secrets as projected volumes and does not inject Redis credentials via env vars.

- HoWL:
  - `internal/controller/cluster/pods.go` (projected secret volumes at `/projected`, TLS at `/tls`)
- OT:
  - `internal/k8sutils/statefulset.go` includes env-var secret injection for `REDIS_PASSWORD` via `SecretKeyRef`.

Why this matters: file-mounted secrets are generally preferred over env-var exposure.

## 6) Richer Per-Pod Status for Operations

HoWL tracks per-pod status in a map keyed by pod name, including role, connectivity, replication offsets, and timestamps.

- HoWL:
  - `api/v1/rediscluster_types.go` (`instancesStatus map[string]InstanceStatus`)
  - `internal/controller/cluster/status.go` (poll + status update)
- OT:
  - Status is comparatively coarser in key APIs (`masterNode`, cluster state/reason, ready leader/follower counts).

Why this matters: better incident debugging and more precise automation logic.

## 7) Avoids StatefulSet Immutability Constraints

HoWL directly manages Pods and PVCs instead of relying on StatefulSets for data pods.

- HoWL:
  - `internal/controller/cluster/pods.go`, `internal/controller/cluster/pvcs.go`
- OT:
  - StatefulSet-heavy lifecycle (`internal/k8sutils/statefulset.go`) with explicit handling of immutable `volumeClaimTemplates` and optional recreate strategy.

Why this matters: HoWL can enforce Redis-specific ordering and behavior directly, instead of fitting everything into generic StatefulSet mechanics.

## Where OT Is Strong

This comparison is not "OT is bad". OT has clear strengths:

- Broad mode support (`standalone`, `replication`, `sentinel`, `cluster`) in a single ecosystem.
- Mature StatefulSet-native workflows and familiar Kubernetes operational model.
- Strong built-in metrics patterns with `redis-exporter` integration and Prometheus/Grafana documentation.

## Practical Decision Guide

Choose HoWL if your priority is:

- deterministic failover behavior,
- split-brain prevention depth,
- controlled primary upgrades,
- direct per-pod operational control.

Choose OT if your priority is:

- broader topology options under one operator,
- StatefulSet-native workflows,
- existing team familiarity with OT ecosystem and tooling.
