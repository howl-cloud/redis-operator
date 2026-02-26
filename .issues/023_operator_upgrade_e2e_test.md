---
id: 23
title: "Operator upgrade E2E test"
priority: p1
type: testing
labels: [production-readiness, testing, operations, upgrades]
created: 2026-02-23
updated: 2026-02-24
depends_on: [7]
completed: false
---

## Summary

Issue #7 documents the manual runbook for operator upgrades. There is no automated test that verifies upgrading the operator binary does not disrupt running clusters. Without this, every operator release carries unknown risk — a regression in the reconciler, a changed API assumption, or a webhook incompatibility could silently break existing clusters during an upgrade.

## Why this is an issue

Operator upgrades happen while clusters are live. Unlike a stateless application upgrade, a broken operator upgrade can:
- Leave clusters in a permanently stuck reconciliation loop
- Trigger an unintended rolling update of Redis pods (version change in pod spec)
- Break the webhook PKI rotation if certificate format changes
- Lose in-flight status updates during the reconciler restart

Users expect to upgrade the operator with zero impact on running Redis clusters. This is only verifiable through an automated test that simulates the upgrade path.

## CloudNativePG equivalent

CNPG's CI runs E2E tests across a matrix of:
- Current PostgreSQL versions × current Kubernetes versions
- An upgrade-specific test suite that:
  1. Installs an older operator version
  2. Creates clusters and verifies they are healthy
  3. Upgrades the operator to the new version (replace Deployment image)
  4. Verifies all clusters remain healthy throughout the upgrade
  5. Verifies no unintended pod restarts occurred
  6. Verifies webhook functionality is restored after the new operator pod is ready

CNPG also tests in-place instance manager updates (the binary hot-swap feature) as part of this suite.

## How to implement in the Redis operator

1. **Test structure** (`test/e2e/operator_upgrade_test.go`):
   ```
   Given operator v<N-1> is installed (previous release image)
   And a RedisCluster "upgrade-test" is Healthy with 3 instances
   And I write 100 keys via redis-cli
   When I upgrade the operator Deployment to v<N> (current build)
   Then the operator pod restarts and becomes Ready
   And the RedisCluster "upgrade-test" remains in Healthy phase throughout
   And no Redis pods were restarted (verify via pod creation timestamps)
   And all 100 keys are still present
   And I can create a new RedisCluster post-upgrade
   And webhooks accept valid requests
   ```

2. **Image tagging in CI**: The build pipeline must produce and push a versioned image for the previous release. Use `git describe --tags` to derive the version. The upgrade test pulls the previous tag from the registry (or loads it into Kind directly).

3. **Pod restart detection**: Record all Redis pod creation timestamps before the upgrade. After upgrade, assert none changed — the operator restart must not trigger any pod reconciliation side effects.

4. **Webhook continuity**: Between old operator terminating and new operator starting, there is a brief window where the webhook is unavailable. The test should verify:
   - Existing clusters are unaffected during this window
   - Webhook is functional within 30 seconds of new operator becoming Ready

5. **Rolling update regression**: After upgrade, deliberately trigger a rolling update (change image tag on the cluster) and verify it completes cleanly under the new operator.

6. **Makefile target**: `make test-upgrade` — runs upgrade scenario specifically. Requires two image tags to be available.

## Acceptance Criteria

- [ ] Operator upgrade (image replace) does not restart any Redis pods
- [ ] Clusters remain Healthy throughout the operator upgrade window
- [ ] Webhooks are functional within 30s of new operator readiness
- [ ] Data written before upgrade is readable after upgrade
- [ ] Rolling updates triggered post-upgrade complete successfully
- [ ] Test runs in CI on every release candidate tag

## Notes

This test requires a "previous release" image to be available. For the initial run, use the last tagged release. In CI, this means the release process must: (1) tag and push the release image, (2) then run the upgrade test using that tag as the "before" image and the current build as the "after" image. This sequence is a natural fit for a dedicated `release` CI workflow that runs after the standard PR checks.
