---
id: 19
title: "Structured chaos testing framework"
priority: p2
type: testing
labels: [production-readiness, testing, reliability]
created: 2026-02-23
updated: 2026-02-26
depends_on: [4]
completed: false
---

## Summary

The `test/chaos/` directory exists but contains no test implementations. Issue #4 (chaos and fault injection testing) was completed with manual/scripted chaos scenarios in `test/smoke/`, but there is no structured, repeatable chaos testing framework that can run in CI and exercise fault injection systematically.

## Why this is an issue

The existing smoke tests on Kind validate specific scenarios, but they are not structured as a reusable framework with:
- Declarative fault definitions (pod kill, network partition, disk pressure, CPU throttle)
- Automated validation of invariants after each fault (no split-brain, data preserved, cluster recovers)
- CI integration for regression testing on every PR
- Coverage tracking of which fault scenarios have been tested

Without this, regressions in self-healing behavior can slip in undetected.

## CloudNativePG equivalent

CNPG has a `disruptive` feature type in its E2E test suite:
- Tests tagged with the `disruptive` label exercise fault injection scenarios
- Self-healing tests include: failover, switchover, recovery from degraded states
- Tests run on disposable Kind clusters in CI via GitHub Actions
- Importance levels (0-4) allow selective execution based on CI context
- Infrastructure setup (`hack/setup-cluster.sh`) handles Kind cluster creation with appropriate capabilities

## How to implement in the Redis operator

1. **Test framework**: Use Ginkgo/Gomega (consistent with existing E2E tests) in `test/chaos/`.

2. **Fault injection methods** (no external dependency needed for basic scenarios):
   - **Pod kill**: `kubectl delete pod --force` via client-go
   - **Network partition**: Apply/remove NetworkPolicy that blocks traffic to/from a specific pod
   - **Slow responses**: Use `tc` (traffic control) in a privileged init container, or use Chaos Mesh if available
   - **Disk pressure**: Fill the PVC with a large file via `kubectl exec`
   - **Operator restart**: Delete the operator pod mid-reconciliation

3. **Test scenarios to implement**:

   | Scenario | Invariant to verify |
   |----------|-------------------|
   | Kill primary during writes | Failover completes, no split-brain, writes resume |
   | Kill all replicas simultaneously | Primary continues, replicas rejoin on recreation |
   | Network-partition primary from peers | Primary fenced (issue #16), failover to replica |
   | Operator crash during failover | Reconciler converges on restart |
   | PVC corruption on replica | Replica recreated with fresh data from primary |
   | Rolling update during high load | No write errors, replication intact |
   | Scale down during failover | Operations serialized correctly |
   | Simultaneous pod evictions (node drain) | PDB enforced, at most one pod lost |

4. **Invariant checks** (run after every fault):
   - `WAIT 1 0` on primary confirms replication caught up
   - Only one pod reports `role:master` in `INFO replication`
   - `status.currentPrimary` matches the actual Redis primary
   - All expected keys are present (write known data before fault, verify after)

5. **CI integration**: Add `make test-chaos-kind` target (already exists) that runs the chaos suite on a Kind cluster.

## Acceptance Criteria

- [x] `test/chaos/` contains at least 6 structured chaos test scenarios
- [x] Each test verifies invariants: no split-brain, data integrity, cluster convergence
- [x] Tests run on Kind via `make test-chaos-kind`
- [x] Tests are tagged/labeled for selective execution in CI
- [ ] All tests pass on a clean Kind cluster

## Notes

Start simple — `kubectl delete pod` and NetworkPolicy-based partitions cover the most important scenarios without requiring Chaos Mesh or Litmus. Add external chaos tooling later for more sophisticated faults (clock skew, partial packet loss, etc.). The existing `test/smoke/` scenarios from issue #4 can be migrated into this framework.

## Current Status (2026-02-26)

Implemented and verified:

- 7 structured chaos scenarios exist under `test/chaos/`:
  - `primary_kill_test.go`
  - `replica_failure_test.go`
  - `network_partition_test.go`
  - `primary_isolation_test.go`
  - `operator_restart_test.go`
  - `process_kill_test.go`
  - `rolling_update_test.go`
- Common invariant checks are implemented and used across scenarios:
  - `AssertNoSplitBrain`
  - `AssertDataIntegrity`
  - `AssertReplicationConverged`
  - `AssertOffsetNotRegressed`
- Kind execution is wired through `make test-chaos-kind` -> `test/chaos/run.sh`.
- Selective execution is implemented with Ginkgo labels and `CHAOS_LABEL_FILTER` (`--ginkgo.label-filter` in `test/chaos/run.sh`).

Not yet passing:

- `make test-chaos-kind` on a clean Kind cluster still fails for network-labeled scenarios.

## Lingering Issue Handoff

### Repro

```bash
CHAOS_LABEL_FILTER=network make test-chaos-kind
```

### Current failing specs

1. `test/chaos/primary_isolation_test.go`
   - Fails at wait for restart:
   - `waiting for restart count increase for pod default/chaos-redis-0: ... context deadline exceeded`
2. `test/chaos/network_partition_test.go`
   - Fails data integrity assertion:
   - `expected 1000 keys with prefix "ac2", got 0`

### Runtime evidence already captured

- Debug log file: `.cursor/debug-391a31.log`
- The failover path is now observed during network partition:
  - `primary changed` entries are present (old primary -> new primary).
- The per-cluster Role now includes pod listing (`podsRoleVerbs: "get,list"`), removing the earlier `pods is forbidden` issue for primary isolation checks.
- Primary-isolation scenario still does not observe a restart event within timeout in the failing runs.

### Likely next debugging focus

1. **Primary isolation restart detection**
   - Verify whether the isolated primary is actually being restarted by kubelet (pod UID/start time/recreated pod name), not only container restart count.
   - Verify liveness `/healthz` failure path under isolation in runtime logs.
2. **Network partition data baseline**
   - Validate baseline write durability before fault injection (keys exist before netblock is applied).
   - Confirm the selected "primary pod" for baseline writes and for isolation are the same pod role at fault start.
3. **Service routing and role transitions**
   - Correlate `status.currentPrimary`, pod Redis role (`INFO replication`), and `-leader` service endpoint during the fault window.
