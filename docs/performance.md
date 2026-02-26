# Reconciler Performance Benchmarks

This document captures baseline performance data for `RedisCluster` reconciliation and instance status polling at scale.

## Scope

- Primary benchmark uses the full reconcile path (`ClusterReconciler.Reconcile`) at:
  - 10 clusters
  - 50 clusters
  - 100 clusters
- Assumed 3 data pods per cluster (300 pod status polls at 100 clusters).
- Kept status polling as a supporting microbenchmark (`pollInstanceStatuses`) for isolated HTTP/status-step cost.
- Added controller tuning knobs:
  - `--max-concurrent-reconciles` (default `5`)
  - `--pprof-bind-address` (default `""`, disabled)

## Benchmark Commands

Run full reconcile benchmarks with memory metrics:

```bash
go test -run='^$' -bench='BenchmarkReconcileLoop' -benchmem -benchtime=3s ./internal/controller/cluster
```

Run status-step microbenchmarks:

```bash
go test -run='^$' -bench='BenchmarkPoll' -benchmem -benchtime=2s ./internal/controller/cluster
```

Capture CPU and memory profiles for the 100-cluster case:

```bash
go test -run='^$' -bench='BenchmarkReconcileLoop/clusters=100' -benchmem -benchtime=2s \
  -cpuprofile=/tmp/issue9-cpu.out \
  -memprofile=/tmp/issue9-mem.out \
  ./internal/controller/cluster

go tool pprof -top /tmp/issue9-cpu.out
go tool pprof -top -alloc_space /tmp/issue9-mem.out
```

## Results

### Apple M4 Max (Go test benchmark harness)

From `go test -run='^$' -bench='BenchmarkReconcileLoop' -benchmem -benchtime=3s ./internal/controller/cluster`:

| Benchmark | Latency | Reconciles/s | Status polls/s | API calls/s | Allocations |
| --- | ---: | ---: | ---: | ---: | ---: |
| `BenchmarkReconcileLoop/clusters=10` | `16.48 ms/op` | `606.9` | `1821` | `16993` | `17.93 MB/op`, `151491 allocs/op` |
| `BenchmarkReconcileLoop/clusters=50` | `223.14 ms/op` | `224.1` | `672.2` | `6274` | `360.47 MB/op`, `1954374 allocs/op` |
| `BenchmarkReconcileLoop/clusters=100` | `793.77 ms/op` | `126.0` | `377.9` | `3527` | `1.35 GB/op`, `6922277 allocs/op` |

Supporting status microbenchmark baseline (`go test -run='^$' -bench='BenchmarkPollPodStatus|BenchmarkPollInstanceStatuses/clusters=100' -benchmem -benchtime=2s ./internal/controller/cluster`):

- `BenchmarkPollPodStatus`: `35.8 us/op`, `6813 B/op`, `75 allocs/op`
- `BenchmarkPollInstanceStatuses/clusters=100`: `139.47 ms/op`, `717 reconciles/s`, `2151 status_polls/s`

### Linux amd64 VM (2026-02-26)

This run was executed on the machine used for this benchmark session:

- OS: Linux `6.8.0-1048-gcp` (Ubuntu)
- CPU: `AMD EPYC 7B13` (`4` vCPUs)
- Memory: `15 GiB`
- Go: `go1.25.1`
- kind: `v0.29.0`

Environment/setup steps executed before benchmarking:

- Created a dedicated kind cluster: `kind create cluster --name redis-operator-perf --wait 180s`
- Installed CRDs: `make install`
- Verified controller startup in-cluster context: `timeout 45s go run ./cmd/manager controller --leader-elect=false --webhook-enabled=false --metrics-bind-address=:9090`

From `go test -run='^$' -bench='BenchmarkReconcileLoop' -benchmem -benchtime=3s ./internal/controller/cluster`:

| Benchmark | Latency | Reconciles/s | Status polls/s | API calls/s | Allocations |
| --- | ---: | ---: | ---: | ---: | ---: |
| `BenchmarkReconcileLoop/clusters=10` | `55.40 ms/op` | `180.5` | `541.6` | `5055` | `17.96 MB/op`, `152608 allocs/op` |
| `BenchmarkReconcileLoop/clusters=50` | `751.13 ms/op` | `66.57` | `199.7` | `1864` | `360.89 MB/op`, `1985928 allocs/op` |
| `BenchmarkReconcileLoop/clusters=100` | `2659.08 ms/op` | `37.61` | `112.8` | `1053` | `1.36 GB/op`, `6959267 allocs/op` |

Supporting status microbenchmark baseline (`go test -run='^$' -bench='BenchmarkPoll' -benchmem -benchtime=2s ./internal/controller/cluster`):

- `BenchmarkPollPodStatus`: `144.7 us/op`, `6734 B/op`, `75 allocs/op`
- `BenchmarkPollInstanceStatuses/clusters=10`: `5.56 ms/op`, `1800 reconciles/s`, `5400 status_polls/s`
- `BenchmarkPollInstanceStatuses/clusters=50`: `114.39 ms/op`, `437.1 reconciles/s`, `1311 status_polls/s`
- `BenchmarkPollInstanceStatuses/clusters=100`: `456.61 ms/op`, `219.0 reconciles/s`, `657.0 status_polls/s`

Profiles captured for `BenchmarkReconcileLoop/clusters=100` (`-cpuprofile`, `-memprofile`) on this host show:

- CPU hot spots are primarily GC/runtime scan work (`runtime.scanobject`, `runtime.findObject`) and JSON encode/decode paths.
- Allocation hot spots are dominated by reflection growth (`reflect.growslice`) and Kubernetes deep-copy operations (`PodList.DeepCopyInto`, `PersistentVolumeClaimList.DeepCopyInto`), plus JSON marshal/decode paths.
- No unbounded goroutine growth signatures were observed in this benchmark path.

> Note: This VM has significantly fewer CPU resources than the Apple M4 Max baseline above, so throughput is expectedly lower; treat this section as a machine-local baseline.

### API Server Request Rate

For the full reconcile benchmark, `apiserver_calls/s` is measured by a counting client wrapper around the controller-runtime client and includes CRUD/list/patch/status-subresource operations executed by reconcile sub-steps.

Measured API call rates:

- 10 clusters: ~`16993 calls/s`
- 50 clusters: ~`6274 calls/s`
- 100 clusters: ~`3527 calls/s`

These are synthetic fake-client benchmark rates and should be treated as relative scaling indicators, not production absolute throughput.

## pprof Review Summary

Profiles captured for `BenchmarkReconcileLoop/clusters=100` (`-cpuprofile`, `-memprofile`) show:

- CPU hot spots include full reconcile work (`Reconcile`/`reconcile`, `reconcilePVCs`, `reconcilePods`, `rollingUpdate`, status polling), plus runtime scheduling/GC and fake client list/deep-copy paths.
- Allocation hot spots are dominated by fake-client deep copies (`PodList`, PVC lists), JSON patch/merge paths, and reflection growth.
- No unbounded goroutine growth signatures were observed in this benchmark path.

Conclusion: no unexpected goroutine leak patterns in the measured path; most allocation pressure comes from benchmark harness/client behavior rather than network I/O.

## Leader Election and Work Queue Review

- Leader election is enabled by default (`--leader-elect=true`, `LeaderElectionID=redis-operator-leader`).
- Cluster reconciler concurrency is now configurable via `--max-concurrent-reconciles` (default `5`), set on controller options.
- Controller-runtime default workqueue rate limiter remains in place for error retries (`ItemExponentialFailureRateLimiter` behavior). No additional custom rate limiter is required for the current periodic 30s requeue model.

## Supported Scale and Sizing Recommendations

### Maximum Supported Clusters per Operator Instance

Current validated target: **100 clusters per operator instance** (3 pods/cluster), with measurable latency/allocation degradation at that scale.

Observed trend from 10 -> 50 -> 100 clusters shows strong growth in reconcile latency and allocation pressure; treat 100 as the current upper bound for one operator instance unless additional optimization (for example, bounded-concurrency status polling) is introduced.

### Recommended Operator Resources

| Managed clusters | CPU request | CPU limit | Memory request | Memory limit |
| --- | ---: | ---: | ---: | ---: |
| Up to 10 | `100m` | `500m` | `128Mi` | `256Mi` |
| Up to 50 | `250m` | `1000m` | `512Mi` | `1Gi` |
| Up to 100 | `500m` | `2000m` | `1Gi` | `2Gi` |

Notes:

- Chart defaults (`100m/128Mi` request, `500m/256Mi` limit) are appropriate for small clusters.
- For 50+ managed clusters, increase memory requests/limits to reduce GC pressure during reconcile bursts.

## Follow-up Optimization

`pollInstanceStatuses` currently polls pods sequentially. A future optimization is bounded parallel polling (for example, `errgroup` + semaphore) to reduce worst-case status poll latency at higher cluster counts.
