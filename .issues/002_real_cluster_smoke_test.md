---
id: 2
title: "Real cluster smoke test"
priority: p0
type: testing
labels: [production-readiness, testing, infrastructure]
created: 2026-02-19
updated: 2026-02-19
depends_on: [1]
---

## Summary

Every line of production code was generated and unit-tested against fake clients. The operator has never reconciled a real Kubernetes cluster, the instance manager has never spawned a real `redis-server`, and no feature has been exercised end-to-end against live infrastructure.

## Acceptance Criteria

- [ ] Operator deployed to a real Kubernetes cluster (kind, k3d, or cloud) using the Helm chart
- [ ] `RedisCluster` with `instances: 1` creates a running Pod and PVC
- [ ] `RedisCluster` with `instances: 3` creates 3 Pods and PVCs, elects a primary, connects replicas
- [ ] `-leader` Service routes to the primary Pod
- [ ] `-replica` Service routes to replica Pods
- [ ] `kubectl exec` into a Pod confirms `redis-cli ping` returns `PONG`
- [ ] Scale up from 1 → 3 instances works
- [ ] Scale down from 3 → 1 works (replicas removed, primary retained)
- [ ] Delete a replica Pod — it is recreated and rejoins replication
- [ ] Any bugs found during smoke testing are fixed before marking this done

## Notes

Bugs are expected here. The `resolvePodIP` function currently uses DNS (`podName.namespace.svc.cluster.local`) rather than fetching the Pod's `status.podIP` — this may not work for pod-to-pod communication and should be validated.
