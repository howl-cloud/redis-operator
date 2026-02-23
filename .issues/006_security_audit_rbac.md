---
id: 6
title: "Security audit and RBAC tightening"
priority: p1
type: security
labels: [security, production-readiness]
created: 2026-02-19
updated: 2026-02-19
depends_on: [1]
completed: true
---

## Summary

The operator's `ClusterRole` grants broad access to pods, PVCs, services, secrets, and events cluster-wide. The RBAC has not been audited for least-privilege. Additionally, the instance manager's per-cluster RBAC (`reconcileRBAC`) is currently a no-op stub.

## Acceptance Criteria

- [x] Audit `config/rbac/role.yaml` — remove any permissions not required by the reconciliation loop
- [x] Instance manager RBAC: implement `reconcileRBAC` to create a per-cluster `Role` and `RoleBinding` scoped to the cluster's namespace with only the permissions the instance manager needs (read `RedisCluster`, patch `RedisCluster/status`)
- [x] Operator deployment runs as a non-root user (`securityContext.runAsNonRoot: true`)
- [x] Pod security context set on generated Redis pods (`runAsNonRoot`, `readOnlyRootFilesystem` where possible, `allowPrivilegeEscalation: false`)
- [x] Secrets are never logged (audit all `logger.Info` and `logger.Error` calls for accidental secret value inclusion)
- [x] Scan image with Trivy or Snyk — no critical CVEs in final image
- [x] `NetworkPolicy` manifest provided in `config/` to restrict pod-to-pod traffic to Redis port (6379) and instance manager port (8080)

## Notes

The current operator watches all namespaces. Consider adding a `--namespace` flag to restrict to a single namespace for tenancy isolation.
