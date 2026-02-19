---
id: 13
title: "OLM bundle for OperatorHub"
priority: p3
type: feature
labels: [distribution, feature]
created: 2026-02-19
updated: 2026-02-19
depends_on: [1, 6]
---

## Summary

The operator is not listed on OperatorHub and has no Operator Lifecycle Manager (OLM) bundle. OLM bundles are required for distribution through the Red Hat Ecosystem Catalog, OpenShift, and any cluster that uses OLM for operator lifecycle management.

## Acceptance Criteria

- [ ] OLM bundle generated using `operator-sdk generate bundle`
- [ ] `ClusterServiceVersion` (CSV) filled out: description, icon, maintainers, links, install modes, RBAC requirements
- [ ] Bundle validated: `operator-sdk bundle validate ./bundle`
- [ ] Bundle pushed to a bundle image and tested via OLM on a local cluster (`operator-sdk run bundle`)
- [ ] Scorecard tests pass (`operator-sdk scorecard`)
- [ ] Submission PR opened against `k8s-operatorhub/community-operators` or `redhat-openshift-ecosystem/community-operators-prod`

## Notes

OLM bundles require the security audit (issue #6) to be complete — the CSV must accurately declare all required RBAC permissions. Attempting to publish before RBAC is tightened will result in reviewers requesting changes.
