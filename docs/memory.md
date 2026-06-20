# Memory and eviction

This page explains how Kubernetes memory limits, Redis `maxmemory`, and eviction
policies interact, and how to configure them safely with `spec.memory`.

## The three layers

| Layer | Field | Enforced by | Failure mode when exceeded |
|---|---|---|---|
| Container memory limit | `spec.resources.limits.memory` | Linux cgroups / kubelet | **OOM-kill** — the container is terminated abruptly |
| Redis memory ceiling | `spec.memory.maxMemory` / `maxMemoryPercent` → `maxmemory` | redis-server | Eviction or write rejection, per policy |
| Eviction policy | `spec.memory.maxMemoryPolicy` → `maxmemory-policy` | redis-server | Defines *which* keys are evicted, or none |

The key relationship: **`maxmemory` must sit below the container limit.** When it
does, Redis reacts to memory pressure on its own terms (predictable) before the
kernel ever steps in (unpredictable). Without `maxmemory`, the container limit is
the *only* ceiling, and hitting it means an OOM-kill — a hard restart with
potential data loss on the affected pod.

## Why headroom matters

Set `maxmemory` strictly below the limit, not equal to it. Redis needs memory
*beyond* the dataset for:

- **Replication buffers** — the primary buffers writes for replicas.
- **Copy-on-write during persistence** — `BGSAVE` and AOF rewrite fork the
  process; pages modified during the fork are duplicated.
- **Fragmentation** — the allocator (jemalloc) holds more RSS than the logical
  dataset size; `redis_mem_fragmentation_ratio` tracks this.

A common starting point is **75%** of the container limit. Workloads that write
heavily during persistence may need more headroom. The webhook emits a warning
when the resolved `maxmemory` is ≥90% of the limit.

## Configuration

Use first-class `spec.memory` fields, not raw `spec.redis` keys. The operator
keeps them consistent with the container limit, applies them live via
`CONFIG SET` (no restart), and validates unsafe combinations.

### Percent of the container limit (recommended)

```yaml
spec:
  resources:
    limits:
      memory: 2Gi
  memory:
    maxMemoryPercent: 75      # maxmemory = 1536Mi
    maxMemoryPolicy: allkeys-lru
```

`maxMemoryPercent` scales automatically if the limit changes. It requires
`spec.resources.limits.memory` to be set.

### Explicit value

```yaml
spec:
  resources:
    limits:
      memory: 2Gi
  memory:
    maxMemory: 1536Mi
    maxMemoryPolicy: noeviction
```

`maxMemory` and `maxMemoryPercent` are mutually exclusive.

## Eviction policies

`maxMemoryPolicy` defaults to **`noeviction`**: at the ceiling, writes fail with
an error and reads still succeed. This turns an OOM-kill into a recoverable,
visible condition *without* silently discarding data — the safe default for a
datastore.

| Policy | Behavior | Use when |
|---|---|---|
| `noeviction` (default) | Reject writes at the limit | Datastore / source of truth |
| `allkeys-lru` / `allkeys-lfu` | Evict any key by recency/frequency | Pure cache |
| `volatile-lru` / `volatile-lfu` / `volatile-ttl` / `volatile-random` | Evict only keys with a TTL | Mixed cache + persistent keys |
| `allkeys-random` / `volatile-random` | Evict random keys | Rarely; uniform access patterns |

## Validation

The admission webhook enforces:

- `maxMemory` and `maxMemoryPercent` are mutually exclusive.
- `maxMemoryPercent` requires `spec.resources.limits.memory`.
- `maxMemory` must not exceed the container limit (rejected outright).
- `maxmemory` / `maxmemory-policy` cannot be set in both `spec.memory` and
  `spec.redis`.

And warns (non-fatal) when:

- A memory limit is set but no `maxmemory` is configured anywhere (OOM-kill risk).
- The resolved `maxmemory` leaves <10% headroom under the limit.

## Monitoring

Once `maxmemory` is configured, the bundled `RedisMemoryUsageHigh` alert becomes
meaningful — it is gated on `redis_maxmemory_bytes > 0` and fires on
`redis_used_memory_bytes / redis_maxmemory_bytes` crossing a threshold. With no
`maxmemory`, that ratio is undefined and the alert never fires, so a cluster can
walk straight into an OOM-kill unalerted. Watch `redis_evicted_keys_total` to
confirm eviction is happening as intended, and `redis_mem_fragmentation_ratio`
to size headroom. See [monitoring.md](monitoring.md) for the full alert set.
