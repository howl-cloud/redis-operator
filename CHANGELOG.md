# Changelog

All notable changes to this project are documented in this file.

The format follows Keep a Changelog, and this project adheres to Semantic Versioning.

## [Unreleased]

### Fixed
- Changing `spec.replicasPerShard` on a running cluster-mode `RedisCluster` no longer reinterprets existing primaries as replicas (#36). Shard membership follows the live topology. A slot owner is the primary of the shard whose range it serves, replicas follow the primary they replicate from, and only empty pods are placed. A Redis-level failover keeps the shard index with the new slot owner, so reshard no longer moves slots back to the old primary.
- Lowering `spec.replicasPerShard` no longer deletes a pod that owns slots. Scale-down still removes the highest ordinals, which can be primaries after a failover or on the old contiguous layout. The operator hands the shard to a surviving pod first. The heir replicates from the doomed primary, takes over with `CLUSTER FAILOVER`, and pod deletion waits until that finishes. `ShardHandover` events record both steps.
- Planned cluster handovers fence the old primary before promotion and clear the fence only after both nodes confirm the transfer. The instance manager keeps Redis alive for coordinated failover. Emergency fences still stop Redis.
- Replica scale-down rebalances surviving replicas to `spec.replicasPerShard`, including legacy layouts with Redis replica migration disabled. Pod deletion uses numeric ordinals, so pods 10 and above come off in the same order the handover planner assumes.
- Pod labels `redis.io/shard`, `redis.io/shard-role`, and `redis.io/role` are rewritten each reconcile from the observed topology. `status.shards` is derived the same way, so they match what Redis reports.
- The instance manager no longer overwrites the operator's cluster fields (`nodeID`, `slotsServed`, `clusterState`, `currentEpoch`, `lastSeenAt`) in `status.instancesStatus` when it reports replication state. It updates only the fields it observes.
- `CLUSTER REPLICATE` against a pod that has not learned its primary through gossip yet returns 409 from the instance manager and is retried next pass, instead of being logged as a reconcile error. Instance-manager HTTP errors now include the response body.

### Changed
- `status.instancesStatus[*].primaryNodeID` is the cluster node ID a replica follows.
- New cluster-mode clusters place replicas after all primaries (pods `0..shards-1` are primaries, later pods are replicas round-robin across shards). Existing clusters keep their current layout.

## [0.2.6]

### Fixed
- Stop the cluster reconciler from re-triggering itself through its own status writes. Every reconcile stamps a fresh `lastSeenAt` per instance, so each pass wrote the RedisCluster, and that write woke the controller again through its own watch. A steady-state cluster reconciled as fast as the API server could answer — roughly 8 times per second per cluster, indefinitely — instead of once per 30s requeue. Status updates leave the generation alone, so the watch now filters on generation, annotation and label changes; the `redis.io/approve-primary-update` annotation still wakes the controller immediately.
- Only move a condition's `LastTransitionTime` when its `Status` actually changes. Rebuilding the condition set with the current timestamp on every pass made each status patch differ from the stored object, which is what kept the write-wake cycle fed.
- Skip the status patch entirely when nothing changed, so a quiet cluster performs no writes between requeues.
- Requeue explicitly when a reconcile stops early for an in-flight rolling update or PVC-resize restart. Those paths returned an empty result and only resumed because a status write happened to wake the controller; without that accident they would stall until an unrelated event arrived.

## [0.2.5]

### Fixed
- Set a controller `ownerReference` on managed Redis data and sentinel pods, and adopt existing pods that have no controller reference. Without an owner the cluster-autoscaler and `kubectl drain` treat these pods as unmanaged and refuse to evict them, pinning otherwise-empty nodes. Adoption is a metadata-only patch, so running pods are adopted in place without a restart.
- Publish the container image under its bare semantic version (`0.2.5`) in addition to the `v`-prefixed tag, so a chart install that defaults `image.tag` to the chart `appVersion` resolves to an image that exists.

### Changed
- Managed pods are now garbage-collected when their `RedisCluster` is deleted (previously they were orphaned). PersistentVolumeClaims carry no owner reference and are unaffected, so retained data survives cluster deletion.

## [0.2.4]

### Fixed
- Stage the system CA bundle into Redis pods and set `SSL_CERT_FILE` so the in-pod backup uploader can verify TLS to cloud object storage (the Redis image ships no trust store); fail pod startup if the bundle cannot be staged.

## [0.2.3]

### Fixed
- Treat Redis `loadmodule` directives as restart-only config so module paths in `spec.redis` are not sent through live `CONFIG SET` reconciliation.

## [0.2.2]

### Fixed
- Seed `/data/users.acl` from `spec.aclConfigSecret` before starting Redis so clusters with an ACL file configured do not fail startup before the instance reconciler can run `ACL LOAD`.

### Changed
- Run independent setup steps in the kind smoke, chaos, and upgrade GitHub Actions workflows in parallel.

## [0.2.1]

### Added
- TLS support for `sentinel` mode: data pods, Sentinel-to-Redis, and Redis-to-Redis traffic are encrypted, and the operator queries Sentinel over TLS. Enable with `spec.tlsSecret` + `spec.caSecret` on a cluster created as `sentinel`. See `docs/tls.md`.

### Changed
- TLS certificate rotation now also applies to Sentinel pods without a restart (live `CONFIG SET` reload), matching data pod behavior.

### Migration Notes
- TLS in `sentinel` mode is supported only for clusters created as `sentinel`. The in-place `standalone` → `sentinel` migration still cannot involve TLS (on either the source or target spec); use the backup/recreate path. See `docs/runbooks/standalone-to-sentinel-migration.md`.
- No CRD schema fields were removed in this release; existing `redis.io/v1` resources remain compatible.

## [0.2.0]

### Added
- Ephemeral Redis data volumes via `spec.storage.type: emptyDir` for pod-local storage (data is lost when a pod is recreated).
- Azure Blob Storage backup and restore support (`spec.destination.azure` on `RedisBackup` and bootstrap restore).
- `spec.memory` on `RedisCluster` for first-class `maxmemory` and eviction policy configuration, kept consistent with container memory limits.
- In-place `standalone` → `sentinel` migration by editing `spec.mode` (requires at least 3 instances); see `docs/runbooks/standalone-to-sentinel-migration.md`.
- Optional operator-published connection Secret via `spec.connectionSecret`, with rendered host, URL, password, and mode-specific endpoints.
- Cron schedule validation for `RedisScheduledBackup` resources at admission time.
- Service contract documentation in `docs/service-contract.md` describing operator-managed Services, labels, and internal annotations.

### Changed
- `RedisScheduledBackup` history limits (`successfulBackupsHistoryLimit`, `failedBackupsHistoryLimit`) now document that they prune `RedisBackup` Kubernetes resources only, not remote backup artifacts.

### Migration Notes
- Prefer `spec.memory` over setting `maxmemory` / `maxmemory-policy` directly in `spec.redis` so the operator can keep memory settings aligned with container limits.
- To upgrade a running `standalone` cluster to `sentinel`, scale `spec.instances` to at least 3 and set `spec.mode: sentinel`; other mode transitions remain unsupported.
- Remote backup artifact retention is outside the operator; use S3 or Azure Blob lifecycle policies to expire old objects.
- No CRD schema fields were removed in this release; existing `redis.io/v1` resources remain compatible.

## [0.1.0]

### Added
- Initial Redis operator release with `RedisCluster`, `RedisBackup`, and `RedisScheduledBackup` CRDs.
- Reconciliation of pods, PVCs, services, secrets, and PDB resources.
- Automatic failover flow with fencing and promotion.
- Sentinel mode support.
- In-pod instance manager process for Redis lifecycle and health endpoints.
