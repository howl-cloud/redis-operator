---
id: 24
title: "Cross-cluster replication for disaster recovery"
priority: p1
type: feature
labels: [production-readiness, high-availability, disaster-recovery, replication]
created: 2026-02-23
updated: 2026-02-23
depends_on: [14, 15]
completed: false
---

## Summary

The operator provides HA within a single Kubernetes cluster but has no story for disaster recovery across clusters or regions. A full Kubernetes cluster failure — AZ outage, cloud provider incident, accidental cluster deletion — results in total, permanent data loss. There is no mechanism to run a standby Redis cluster in a second region that continuously replicates from the primary.

## Why this is an issue

HA and DR are different problems:
- **HA** (what this operator has): survives pod failures, node failures, primary crashes within one cluster
- **DR** (what's missing): survives the loss of the entire Kubernetes cluster or region

Without DR, this operator cannot be used for any data that must survive a regional outage. This rules out most Tier-1 production use cases (user session stores, rate limiting, feature flags, leaderboards) where loss of the entire region should not mean data loss.

## CloudNativePG equivalent

CNPG calls this **Distributed Topology** or **Replica Clusters**:
- A `Cluster` resource in a second Kubernetes cluster is declared as `spec.replica.enabled: true` with a `spec.replica.source` pointing to a `ExternalCluster` definition
- The replica cluster continuously recovers WAL from the primary cluster's object storage (async) or via streaming replication (sync, requires network connectivity)
- Promotion of the replica cluster to primary is a manual, declarative operation: set `spec.replica.primary` to the replica cluster name
- The replica cluster can serve read-only queries while in replica mode
- Both async (object storage WAL shipping) and sync (streaming replication) topologies are supported, and can be combined

## How to implement in the Redis operator

### Phase 1: Read replica cluster via streaming replication

1. **New CRD or spec field**: Add `spec.replicaMode` to `RedisCluster`:
   ```yaml
   spec:
     replicaMode:
       enabled: true
       source:
         clusterName: "primary-cluster"
         host: "primary-cluster-leader.production.svc.cluster.local"
         port: 6379
         authSecretName: "primary-auth"
   ```
   When `replicaMode.enabled` is true, the entire cluster acts as a replica of an external Redis primary. All instances in this cluster replicate from the external primary.

2. **Instance manager**: On startup, if `replicaMode.enabled`, configure the designated local primary as `REPLICAOF <external-host> <port>` instead of standing up as an independent primary.

3. **Service behavior**: The `-leader` service of a replica-mode cluster points to the local instance running `REPLICAOF`, but clients should be warned it is read-only. Consider annotating the Service with `redis.io/replica-mode: "true"`.

4. **Promotion**: Add a `spec.replicaMode.promote: true` field (or a dedicated annotation). When set, the operator:
   - Removes the `REPLICAOF` config from the local primary
   - Issues `REPLICAOF NO ONE`
   - Clears `replicaMode.enabled`
   - Updates the cluster to operate as an independent primary
   - Records a `ReplicaClusterPromoted` event

### Phase 2: Async replication via shared object storage (higher durability)

Requires issue #14 (backup) to be complete. Uses periodic backups + continuous AOF shipping to object storage as the replication medium, eliminating the need for direct network connectivity between clusters.

### Topology example

```
Region A (primary K8s cluster)
  RedisCluster "prod-primary"
    mode: sentinel, instances: 3
    backup: S3 bucket s3://redis-backups/prod

Region B (DR K8s cluster)
  RedisCluster "prod-dr"
    replicaMode.enabled: true
    replicaMode.source.host: prod-primary-leader.region-a.example.com
    replicaMode.source.authSecretName: dr-auth

Promotion (on region A failure):
  kubectl patch rediscluster prod-dr --patch '{"spec":{"replicaMode":{"promote":true}}}'
```

## Acceptance Criteria

- [ ] `spec.replicaMode.enabled: true` causes all instances to replicate from the external source
- [ ] Replica cluster serves read-only queries while in replica mode
- [ ] Setting `spec.replicaMode.promote: true` promotes the cluster to an independent primary
- [ ] Promotion is recorded as an event and status condition
- [ ] Webhook rejects `replicaMode.source` with missing or invalid fields
- [ ] E2E test: create primary cluster → write data → create replica cluster → verify data replicates → promote replica → write to promoted cluster → verify

## Notes

Phase 1 (streaming replication) is straightforward to implement since Redis supports `REPLICAOF` to any external host natively. The main complexity is lifecycle management: what happens when the external primary is temporarily unreachable? Redis will reconnect and re-sync automatically, so the operator mostly needs to set the initial config and handle promotion.

Phase 2 (object storage shipping) is significantly more complex and should be a separate issue. Phase 1 alone covers the core DR use case for clusters with network connectivity between regions (VPC peering, VPN, etc.).

TLS (issue #15) should be completed before this issue, since cross-cluster replication over the public internet requires encrypted connections.
