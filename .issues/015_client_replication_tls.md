---
id: 15
title: "Client and replication TLS support"
priority: p0
type: feature
labels: [production-readiness, security, tls]
created: 2026-02-23
updated: 2026-02-23
depends_on: []
completed: false
---

## Summary

The `RedisCluster` CRD has `tlsSecret` and `caSecret` fields in the spec, but these are not wired through to actually configure `redis-server` with TLS. Client-to-Redis and replica-to-primary connections are unencrypted. The operator only manages TLS for its own webhook PKI, not for Redis data-plane traffic.

## Why this is an issue

Any production deployment in a shared or regulated environment requires encryption in transit. Without TLS:
- Redis passwords are sent in cleartext over the network
- Replication traffic (full dataset on initial sync) is unencrypted
- Compliance frameworks (SOC 2, PCI-DSS, HIPAA) cannot be satisfied
- Kubernetes network policies provide isolation but not encryption

## CloudNativePG equivalent

CNPG enforces TLS by default for all connections:
- **Operator-managed mode**: Self-signed CA, 90-day server certificates, auto-renewed 7 days before expiration with zero downtime
- **User-provided mode**: Reference external Kubernetes Secrets (`serverCASecret`, `serverTLSSecret`, `clientCASecret`, `replicationTLSSecret`), supports cert-manager integration
- TLS v1.3 required between nodes
- Certificate-based authentication for the `streaming_replica` user via `pg_hba.conf`

## How to implement in the Redis operator

1. **Wire existing spec fields**: `spec.tlsSecret` should reference a Secret containing `tls.crt` and `tls.key`. `spec.caSecret` should reference a Secret containing `ca.crt`. Mount these as projected volumes.

2. **Redis config generation**: When TLS secrets are present, the instance manager should generate `redis.conf` with:
   ```
   tls-port 6379
   port 0
   tls-cert-file /tls/tls.crt
   tls-key-file /tls/tls.key
   tls-ca-cert-file /tls/ca.crt
   tls-auth-clients optional
   tls-replication yes
   ```

3. **Operator-managed mode** (optional, phase 2): If no `tlsSecret` is provided but `spec.tls.enabled: true`, the operator generates a CA and per-pod certificates, similar to how webhook PKI works in `internal/cmd/manager/controller/pki.go`. Reuse the existing PKI infrastructure.

4. **Service ports**: Update Service definitions to expose the TLS port. Consider supporting both TLS and non-TLS ports during migration.

5. **Health checks**: Update the instance manager's HTTP client and Redis client to use TLS when connecting to Redis.

6. **Replication**: Ensure `REPLICAOF` commands use TLS connections when `tls-replication yes` is set.

## Acceptance Criteria

- [ ] Setting `spec.tlsSecret` and `spec.caSecret` configures Redis with TLS
- [ ] Client connections without TLS are rejected when TLS is enabled
- [ ] Replication traffic is encrypted (`tls-replication yes`)
- [ ] Instance manager health checks work over TLS
- [ ] Certificate rotation (new Secret data) triggers a config reload without pod restart
- [ ] E2E test: create TLS-enabled cluster, connect with `redis-cli --tls`, verify replication works

## Notes

Redis 6.0+ supports TLS natively. The `tls-replication yes` directive encrypts replica-to-primary traffic automatically. Consider making TLS opt-in initially (unlike CNPG which defaults to on) since many Redis deployments run in trusted networks where TLS overhead is unwanted.
