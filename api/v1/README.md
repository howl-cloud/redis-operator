# api/v1

CRD type definitions for the `redis.io/v1` API group.

## Types

| File | Kind | Scope | Description |
|------|------|-------|-------------|
| `rediscluster_types.go` | `RedisCluster` | Namespaced | Core cluster definition: instances, mode, storage, replication |
| `redisbackup_types.go` | `RedisBackup` | Namespaced | On-demand backup execution and status |
| `redisscheduledbackup_types.go` | `RedisScheduledBackup` | Namespaced | Cron-based scheduled backups |
| `groupversion_info.go` | — | — | API group registration (`redis.io/v1`) |
| `zz_generated.deepcopy.go` | — | — | Generated DeepCopy methods (do not edit) |

## RedisCluster

Primary CRD. Defines the desired state of a Redis replication cluster.

Key spec fields:
- `instances` — total number of pods (1 primary + N-1 replicas)
- `mode` — `standalone`, `sentinel`, or `cluster` (hash-slot sharding)
- `imageName` — Redis container image (default: `redis:7.2`)
- `storage` — PVC template for `/data`
- `redis` — redis.conf parameters
- `minSyncReplicas` / `maxSyncReplicas` — synchronous replication quorum
- `resources` — CPU/memory requests and limits
- `nodeSelector` — constrain pods to specific nodes
- `affinity` — pod affinity/anti-affinity rules (e.g. spread replicas across zones)
- `tolerations` — allow scheduling onto tainted nodes
- `topologySpreadConstraints` — enforce even distribution across failure domains (recommended: one constraint on `topology.kubernetes.io/zone` with `maxSkew: 1`)
- `enablePodDisruptionBudget` — when `true` (default), operator creates a PDB with `minAvailable: max(1, instances-1)` to protect the cluster during node drain

**Secret references in spec** (all are `LocalObjectReference` — name of a `Secret` in the same namespace):

| Field | Secret key | Description |
|-------|-----------|-------------|
| `authSecret` | `password` | Redis `requirepass` / `AUTH` password |
| `aclConfigSecret` | `acl` | Full Redis ACL rules file (Redis 6+, mounted at `/data/users.acl`) |
| `tlsSecret` | `tls.crt`, `tls.key` | Server TLS certificate and private key |
| `caSecret` | `ca.crt` | CA certificate for TLS client verification |
| `backupCredentialsSecret` | varies | Object storage credentials for backup (S3/GCS/Azure) |

If `authSecret` is not provided, the operator generates a random password and stores it in `<cluster-name>-auth` automatically.

Key status fields:
- `currentPrimary` — pod name of the current primary
- `phase` — human-readable cluster phase (e.g. `Healthy`, `FailingOver`)
- `conditions` — standard `metav1.Condition` slice (use with `kubectl wait`)
- `instancesStatus` — per-pod status map reported by instance managers
- `readyInstances` — count of pods passing readiness probes
- `healthyPVC`, `danglingPVC` — PVC lifecycle tracking
- `secretsResourceVersion` — map of secret name → `ResourceVersion`; used to detect secret rotation and trigger reconciliation

## Condition Types

| Type | Description |
|------|-------------|
| `Ready` | Cluster is operational with a reachable primary |
| `PrimaryAvailable` | Primary pod is running and accepting connections |
| `ReplicationHealthy` | All replicas are connected and lag is within threshold |
| `LastBackupSucceeded` | Most recent backup completed successfully |

## Code Generation

Types are annotated with `controller-gen` markers. Regenerate with:

```bash
make generate  # runs controller-gen to update zz_generated.deepcopy.go
make manifests # runs controller-gen to update config/crd/bases/
```
