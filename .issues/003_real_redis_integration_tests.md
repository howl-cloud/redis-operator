---
id: 3
title: "Real Redis integration tests"
priority: p0
type: testing
labels: [production-readiness, testing]
created: 2026-02-19
updated: 2026-02-19
depends_on: [1]
---

## Summary

All unit tests use `miniredis`, which does not implement Redis replication (`PSYNC`/`SYNC`), `ACL LOAD`, `CONFIG SET requirepass`, or TLS. The most critical code paths in the instance manager are validated only against a fake. Integration tests against a real `redis-server` process are required before this operator handles production data.

## Acceptance Criteria

- [ ] Integration test suite using `testcontainers-go` (or a locally spawned `redis-server`) that tests:
  - `replication.GetInfo` parses real `INFO replication` output
  - `replication.Promote` (`REPLICAOF NO ONE`) promotes a replica to primary
  - `replication.SetReplicaOf` configures replication between two real Redis instances
  - Instance manager `reconcileSecrets` applies `CONFIG SET requirepass` and the new password is enforced
  - Instance manager `reconcileSecrets` writes an ACL file and `ACL LOAD` takes effect
  - `writeRedisConf` produces a `redis.conf` that starts `redis-server` without errors
- [ ] Tests run in CI (tagged `//go:build integration` or a separate `make test-integration` target)
- [ ] Tests are skipped gracefully when Docker is unavailable (CI without Docker)

## Notes

The `miniredis` tests remain valuable as fast unit tests. Integration tests are additive — they validate the real Redis wire protocol behavior that `miniredis` approximates.
