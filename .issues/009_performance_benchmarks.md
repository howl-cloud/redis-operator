---
id: 9
title: "Reconciler performance benchmarks"
priority: p2
type: testing
labels: [performance, testing]
created: 2026-02-19
updated: 2026-02-19
depends_on: [2]
---

## Summary

The reconciler has not been profiled or benchmarked at scale. It is unknown how the operator performs with many `RedisCluster` resources in a single cluster, or whether the HTTP status poll loop creates excessive API server load at scale.

## Acceptance Criteria

- [ ] Benchmark: operator managing 10, 50, 100 `RedisCluster` resources — measure reconcile loop latency and API server request rate
- [ ] HTTP status poll: confirm that polling all pod IPs on every reconcile cycle does not cause excessive CPU or network overhead at 100+ clusters
- [ ] Leader election and work queue configuration reviewed for thundering herd under load
- [ ] `pprof` profiles captured and reviewed — no unexpected allocations or goroutine leaks
- [ ] Result documented: maximum supported clusters per operator instance before performance degrades, with recommended resource requests/limits for the operator Deployment

## Notes

The `requeueInterval` of 30s means each cluster is reconciled roughly every 30s plus reconcile time. At 100 clusters with 3 pods each, that is 300 HTTP polls every ~30s. Batching or reducing the poll frequency at scale may be needed.
