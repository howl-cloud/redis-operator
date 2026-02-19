---
id: 4
title: "Chaos and fault injection testing"
priority: p0
type: testing
labels: [production-readiness, testing, reliability]
created: 2026-02-19
updated: 2026-02-19
depends_on: [2, 3]
---

## Summary

The split-brain prevention mechanism (fencing annotation + boot-time `currentPrimary` check) is the most critical safety property of this operator. It has only been unit-tested with a fake client. Chaos testing is required to validate that no scenario produces two primaries accepting writes simultaneously.

## Acceptance Criteria

- [ ] **Primary kill**: delete the primary Pod mid-write — operator detects, fences, promotes a replica, former primary rejoins as replica on restart without split-brain
- [ ] **Network partition**: isolate the primary Pod from the API server — operator fences and promotes; isolated primary cannot accept writes due to fencing
- [ ] **Operator restart mid-failover**: kill the operator Pod during step 3 of the failover sequence — system converges correctly on restart
- [ ] **Simultaneous replica failures**: lose all replicas — cluster degrades gracefully, primary continues serving reads/writes
- [ ] **Slow disk / OOM**: inject resource pressure on primary — liveness probe fires, Pod replaced, replication resumes
- [ ] **Rolling update under load**: run `redis-benchmark` during a rolling update — no write errors observed
- [ ] All scenarios validated with `redis-cli WAIT` and offset comparison to confirm no data loss

## Notes

Tooling options: Chaos Mesh, Litmus, or manual `kubectl delete pod` + network policy injection. At minimum, manual kill tests must be run before production. Chaos Mesh is recommended for repeatable CI chaos runs.
