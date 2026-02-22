---
id: 2
title: "Real cluster smoke test"
priority: p0
type: testing
labels: [production-readiness, testing, infrastructure]
created: 2026-02-19
updated: 2026-02-22
depends_on: [1]
completed: true
---

## Summary

Every line of production code was generated and unit-tested against fake clients. The operator has never reconciled a real Kubernetes cluster, the instance manager has never spawned a real `redis-server`, and no feature has been exercised end-to-end against live infrastructure.

## Acceptance Criteria

- [x] Operator deployed to a real Kubernetes cluster (kind, k3d, or cloud) using the Helm chart
- [x] `RedisCluster` with `instances: 1` creates a running Pod and PVC
- [x] `RedisCluster` with `instances: 3` creates 3 Pods and PVCs, elects a primary, connects replicas
- [x] `-leader` Service routes to the primary Pod
- [x] `-replica` Service routes to replica Pods
- [x] `kubectl exec` into a Pod confirms `redis-cli ping` returns `PONG`
- [x] Scale up from 1 → 3 instances works
- [x] Scale down from 3 → 1 works (replicas removed, primary retained)
- [x] Delete a replica Pod — it is recreated and rejoins replication
- [x] Any bugs found during smoke testing are fixed before marking this done

## Notes

Bugs are expected here. The `resolvePodIP` function currently uses DNS (`podName.namespace.svc.cluster.local`) rather than fetching the Pod's `status.podIP` — this may not work for pod-to-pod communication and should be validated.
