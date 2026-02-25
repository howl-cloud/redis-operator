---
id: 16
title: "Runtime primary isolation detection (liveness-based split-brain guard)"
priority: p1
type: feature
labels: [production-readiness, high-availability, split-brain]
created: 2026-02-23
updated: 2026-02-23
depends_on: []
completed: true
---

## Summary

The current split-brain guard only fires at instance startup: if `POD_NAME != status.currentPrimary`, the instance issues `REPLICAOF` before starting `redis-server`. There is no runtime detection — a primary that becomes network-partitioned after startup can continue accepting writes indefinitely, creating a split-brain scenario.

## Why this is an issue

Network partitions happen at runtime, not just at boot. If the primary pod loses connectivity to the Kubernetes API server and all peer pods simultaneously, it has no way to know it's been fenced or that a new primary has been elected. It will continue accepting writes that diverge from the new primary's dataset. When connectivity restores, those writes are silently lost because the former primary rejoins as a replica via `REPLICAOF`.

## CloudNativePG equivalent

CNPG implements **Primary Isolation** as a liveness probe enhancement:
- The primary periodically checks two conditions: (1) can it reach the Kubernetes API server? (2) can it reach any other instance via the instance manager HTTP API?
- If **both** checks fail, the liveness probe returns unhealthy
- Kubernetes kills the pod after the liveness failure threshold
- Configurable via `requestTimeout` and `connectionTimeout`
- This is enabled by default in CNPG

The key insight: a primary that can't reach the API server AND can't reach any peer is likely partitioned. Killing it is safer than letting it accept divergent writes.

## How to implement in the Redis operator

1. **Extend the `/healthz` endpoint** in `internal/instance-manager/webserver/server.go`:
   - If this instance is the primary (check `POD_NAME == currentPrimary`), perform two additional checks:
     - **API server reachability**: Attempt a lightweight K8s API call (e.g., `GET /healthz` on the API server, or read the RedisCluster CR)
     - **Peer reachability**: Attempt `GET /v1/status` on at least one other pod IP from the cluster's expected pod list
   - If both fail, return HTTP 503 (unhealthy)
   - If either succeeds, return HTTP 200 (healthy) — partial connectivity is not a partition

2. **Configuration**: Add `spec.primaryIsolation` with fields:
   - `enabled` (default: `true`)
   - `apiServerTimeout` (default: `5s`)
   - `peerTimeout` (default: `5s`)

3. **Pod spec**: Ensure the liveness probe on Redis pods uses the `/healthz` endpoint with appropriate `failureThreshold` (e.g., 3) and `periodSeconds` (e.g., 10) so a transient blip doesn't kill the primary.

4. **Replica behavior**: Only apply the isolation check to the primary. Replicas losing connectivity is handled by normal replication lag detection.

## Acceptance Criteria

- [x] Primary pod's liveness probe fails when it cannot reach both the K8s API server and all peers
- [x] Pod is killed by kubelet after liveness failure threshold, triggering normal failover
- [x] Transient network blips (< failureThreshold * periodSeconds) do not kill the primary
- [x] Replicas are not affected by the isolation check
- [x] Chaos test: network-partition the primary pod — verify it self-kills and failover proceeds

## Notes

This is the strongest runtime split-brain prevention mechanism available without a distributed consensus protocol. It trades availability (primary dies on full partition) for consistency (no divergent writes). The trade-off is correct for a database operator — better to have brief downtime than silent data loss.
