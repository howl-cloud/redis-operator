# TLS

The operator can run Redis with TLS enabled in **all three modes** —
`standalone`, `sentinel`, and `cluster`. When TLS is on, Redis serves only the
encrypted port (the plaintext listener is disabled), and replication between
pods is encrypted as well.

TLS is **opt-in**: nothing is encrypted unless you set both `spec.tlsSecret` and
`spec.caSecret`.

## Enabling it

You provide two Secrets:

| Secret | Required keys | Purpose |
|---|---|---|
| `spec.tlsSecret` | `tls.crt`, `tls.key` | The server keypair presented by every Redis (and Sentinel) pod. |
| `spec.caSecret` | `ca.crt` | The CA bundle used to verify peers (replication, Sentinel, and operator → Sentinel). |

Both must be set together — the webhook rejects a spec that sets only one.

```yaml
apiVersion: redis.io/v1
kind: RedisCluster
metadata:
  name: my-redis
  namespace: default
spec:
  mode: sentinel
  instances: 3
  storage:
    size: 1Gi
  tlsSecret:
    name: my-redis-tls   # contains tls.crt + tls.key
  caSecret:
    name: my-redis-ca    # contains ca.crt
```

The Secrets are mounted into each pod as a read-only projected volume at `/tls`
(`tls.crt`, `tls.key`, `ca.crt`). They are **never** injected as environment
variables. Rotating the Secret contents is picked up without recreating pods:
data pods reload the new material via `CONFIG SET`, sentinel pods do the same on
a short polling interval, and the operator re-reads the CA bundle on each
handshake. No restart is required to rotate certificates or the CA.

## What it configures

When TLS is enabled, every data pod's `redis.conf` gets:

```
tls-port 6379
port 0                 # plaintext listener disabled
tls-cert-file /tls/tls.crt
tls-key-file /tls/tls.key
tls-ca-cert-file /tls/ca.crt
tls-auth-clients optional
tls-replication yes    # replica → primary link is encrypted
```

The client port number is unchanged (`6379`); it simply requires TLS now.

### Sentinel mode specifics

In `sentinel` mode, the Sentinel pods get the matching TLS configuration on the
Sentinel port (`26379`):

```
tls-port 26379
port 0
tls-cert-file /tls/tls.crt
tls-key-file /tls/tls.key
tls-ca-cert-file /tls/ca.crt
tls-auth-clients optional
tls-replication yes    # Sentinel monitors masters/replicas over TLS
```

Two things follow from this that are worth knowing:

- **The operator queries Sentinel over TLS.** The controller connects directly
  to each Sentinel pod (by pod IP) to read the elected master. It loads `ca.crt`
  from `spec.caSecret` and verifies the Sentinel's certificate chain against it.
  Because the operator dials pod IPs (which are not in the certificate SANs),
  hostname verification is skipped in favor of explicit CA-chain verification —
  the same approach used by the in-pod loopback client.
- **Failover behavior is unchanged.** Sentinel elects a new master and issues
  `REPLICAOF` over TLS exactly as it would in plaintext. You keep the same HA
  guarantees with encryption added.

## Client authentication (`tls-auth-clients optional`)

Both data and Sentinel pods use `tls-auth-clients optional`: clients must speak
TLS, but are **not** required to present a client certificate. Authentication is
still enforced via the Redis password (`requirepass` / `sentinel auth-pass`), so
"optional client cert" is not "unauthenticated."

Mutual TLS (requiring client certificates) is not currently supported. If you
need it, open an issue — it would apply to data and Sentinel pods together.

## Connecting clients

The published [connection Secret](connection-secret.md) automatically switches
its URLs to the `rediss://` scheme when TLS is enabled, and still exposes the
Sentinel discovery keys (`sentinel_host`, `sentinel_port`, `master_name`) in
`sentinel` mode.

`redis-cli` against a data Service:

```bash
redis-cli -h my-redis-leader.default.svc -p 6379 \
  --tls --cacert /path/to/ca.crt -a "$REDIS_PASSWORD" PING
```

A Sentinel-aware client (here, `redis-py`) discovering the master over TLS:

```python
from redis.sentinel import Sentinel

sentinel = Sentinel(
    [("my-redis-sentinel.default.svc", 26379)],
    ssl=True,
    ssl_ca_certs="/path/to/ca.crt",
    sentinel_kwargs={"password": REDIS_PASSWORD},
    password=REDIS_PASSWORD,
)
master = sentinel.master_for("my-redis", ssl=True, ssl_ca_certs="/path/to/ca.crt")
master.set("key", "value")
```

The Sentinel master name is the `RedisCluster` name.

## Limitations

- **No in-place `standalone` → `sentinel` migration with TLS.** Migrating a
  running TLS-enabled standalone cluster to sentinel in place is rejected: the
  primary would be serving TLS while new replicas/sentinels come up, and clearing
  TLS in the same change would strand the migration. Create the cluster as
  `sentinel` from the start, or use the backup/recreate path. See
  [standalone-to-sentinel migration](runbooks/standalone-to-sentinel-migration.md).
- **No mutual TLS.** Client certificates are accepted but not required (see
  above).
- **The operator self-manages webhook PKI** separately; the `tlsSecret`/`caSecret`
  here are for the Redis data plane only and are unrelated to the webhook's
  certificates.
