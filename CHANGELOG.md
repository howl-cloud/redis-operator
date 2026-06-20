# Changelog

All notable changes to this project are documented in this file.

The format follows Keep a Changelog, and this project adheres to Semantic Versioning.

## [Unreleased]

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