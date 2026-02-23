---
id: 19
title: "Structured chaos testing framework"
priority: p2
type: testing
labels: [production-readiness, testing, reliability]
created: 2026-02-23
updated: 2026-02-23
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

- [ ] `test/chaos/` contains at least 6 structured chaos test scenarios
- [ ] Each test verifies invariants: no split-brain, data integrity, cluster convergence
- [ ] Tests run on Kind via `make test-chaos-kind`
- [ ] Tests are tagged/labeled for selective execution in CI
- [ ] All tests pass on a clean Kind cluster

## Notes

Start simple — `kubectl delete pod` and NetworkPolicy-based partitions cover the most important scenarios without requiring Chaos Mesh or Litmus. Add external chaos tooling later for more sophisticated faults (clock skew, partial packet loss, etc.). The existing `test/smoke/` scenarios from issue #4 can be migrated into this framework.
