# webhooks

Defaulting and validation webhook implementations for `RedisCluster`.

## Defaulter (Mutating)

`RedisClusterDefaulter` populates omitted fields with sensible defaults before the resource is persisted:

- `spec.instances` defaults to `1`
- `spec.imageName` defaults to `redis:7.2`
- `spec.mode` defaults to `standalone`
- `spec.storage.size` defaults to `1Gi`
- `spec.resources` defaults to minimal requests (128Mi memory, 100m CPU)

## Validator (Validating)

`RedisClusterValidator` enforces invariants on create and update:

- `spec.instances` must be ≥ 1
- `spec.minSyncReplicas` must be ≤ `spec.instances - 1`
- `spec.maxSyncReplicas` must be ≥ `spec.minSyncReplicas`
- `spec.storage` is immutable after creation (PVC resize must go through the resize flow)
- `spec.mode` is immutable after creation (cannot switch between standalone/sentinel/cluster)

## Files

| File | Description |
|------|-------------|
| `rediscluster_defaulter.go` | `RedisClusterDefaulter` — implements `admission.CustomDefaulter` |
| `rediscluster_validator.go` | `RedisClusterValidator` — implements `admission.CustomValidator` |
