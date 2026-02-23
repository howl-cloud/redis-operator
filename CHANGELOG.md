# Changelog

All notable changes to this project are documented in this file.

The format follows Keep a Changelog, and this project adheres to Semantic Versioning.

## [Unreleased]

### Changed
- Operator upgrades now use a deterministic pod spec hash and the existing rolling update flow to update Redis data pods one at a time (replicas first, primary last via switchover).
- The `redis-operator controller` command now enables leader election by default (`--leader-elect=true`).

### Added
- Upgrade and migration guide in `docs/upgrade.md`, including a `helm upgrade` validation procedure for clusters with existing `RedisCluster` resources.

### Migration Notes
- If your deployment intentionally ran the controller with leader election disabled, pass `--leader-elect=false` explicitly after upgrading.
- Operator image changes now trigger controlled rolling updates of Redis data pods because the init container image is part of the pod spec hash.
- No CRD schema fields were removed in this release; existing `redis.io/v1` resources remain compatible.

## [0.1.0]

### Added
- Initial Redis operator release with `RedisCluster`, `RedisBackup`, and `RedisScheduledBackup` CRDs.
- Reconciliation of pods, PVCs, services, secrets, and PDB resources.
- Automatic failover flow with fencing and promotion.
- Sentinel mode support.
- In-pod instance manager process for Redis lifecycle and health endpoints.
