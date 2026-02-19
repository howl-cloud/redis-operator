# config/crd

Generated CRD YAML manifests. Do not edit by hand.

Regenerate with:

```bash
make manifests
# runs: controller-gen crd paths="./..." output:crd:artifacts:config=config/crd/bases
```

## Files

| File | Description |
|------|-------------|
| `bases/redis.io_redisclusters.yaml` | RedisCluster CRD |
| `bases/redis.io_redisbackups.yaml` | RedisBackup CRD |
| `bases/redis.io_redisscheduledbackups.yaml` | RedisScheduledBackup CRD |
