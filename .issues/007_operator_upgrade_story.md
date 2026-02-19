---
id: 7
title: "Operator upgrade story and CRD versioning"
priority: p1
type: feature
labels: [production-readiness, reliability]
created: 2026-02-19
updated: 2026-02-19
depends_on: [2]
---

## Summary

There is no defined process for upgrading the operator itself. CRDs are at `v1alpha1` with no conversion webhooks or version migration strategy. An in-place upgrade of the operator in a cluster with existing `RedisCluster` resources has not been tested or designed.

## Acceptance Criteria

- [ ] Upgrade path documented: what happens when the operator Deployment is updated to a new image
- [ ] CRD fields that have been added/removed between versions are handled without data loss (additive-only changes preferred; removals require deprecation period)
- [ ] If `v1alpha1` → `v1beta1` or `v1` graduation is planned, conversion webhook scaffolded and tested
- [ ] `helm upgrade` tested against a cluster with existing `RedisCluster` resources — existing clusters continue to run without disruption
- [ ] Operator upgrade does not trigger unintended rolling restarts of healthy Redis pods
- [ ] Leader election enabled and tested so that a zero-downtime operator rollout is possible
- [ ] Migration notes added to `CHANGELOG.md` for any breaking spec changes

## Notes

Kubernetes recommends not removing CRD fields between versions — mark deprecated fields with `// Deprecated` and ignore them in the reconciler rather than removing them.
