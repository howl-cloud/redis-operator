---
id: 5
title: "Implement or explicitly disable Sentinel and Cluster modes"
priority: p0
type: feature
labels: [production-readiness, feature]
created: 2026-02-19
updated: 2026-02-23
depends_on: [1]
completed: true
---

## Summary

`RedisClusterSpec.Mode` accepts `standalone`, `sentinel`, and `cluster` values, but only `standalone` (primary-replica) mode is implemented. The validating webhook does not reject `mode: sentinel` or `mode: cluster`, so a user could create a `RedisCluster` with these modes and get a cluster that silently behaves as standalone.

## Acceptance Criteria

**Option A — Implement Sentinel mode:**
- [x] Instance manager writes `sentinel.conf` and spawns `redis-sentinel` alongside `redis-server` when `mode: sentinel`
- [x] Operator provisions the correct number of sentinel instances
- [x] Failover is delegated to Sentinel rather than the operator's HTTP-based mechanism

**Option B — Implement Redis Cluster mode (sharding):**
- [ ] Operator provisions pods in cluster-aware topology
- [ ] `redis-cli --cluster create` bootstrap or equivalent
- [ ] Re-sharding and slot migration supported

**Option C — Block unsupported modes (minimum acceptable):**
- [x] Validating webhook rejects `mode: sentinel` and `mode: cluster` with a clear error message: `"sentinel and cluster modes are not yet supported; use standalone"`
- [x] `RedisClusterSpec.Mode` type narrowed to document the limitation

## Notes

Option C is the minimum to avoid silent misconfiguration. Options A and B are significant engineering efforts. Choose Option C to unblock production use of standalone mode, and track A/B as separate issues.