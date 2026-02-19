---
id: 10
title: "PKI rotation end-to-end test"
priority: p2
type: testing
labels: [testing, security]
created: 2026-02-19
updated: 2026-02-19
depends_on: [2]
---

## Summary

The self-managed webhook PKI (`internal/cmd/manager/controller/pki.go`) has unit tests, but the full rotation cycle — expiry detection, new cert generation, webhook configuration patching, and the webhook continuing to serve without downtime — has not been tested end-to-end on a real cluster.

## Acceptance Criteria

- [ ] E2E test that deploys the operator, confirms the webhook is functional, fast-forwards the cert expiry (or uses a short `certValidityDays`), and confirms the cert is rotated without webhook downtime
- [ ] Test confirms that the `MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` `caBundle` fields are updated after rotation
- [ ] Webhook continues to serve admission requests during and after rotation (no gap where requests are rejected due to cert mismatch)
- [ ] Rotation is logged and emits a Kubernetes Event on the operator Pod

## Notes

The `needsRenewal` threshold is currently 30 days. A test could set `certValidityDays = 1` and `needsRenewal` threshold to 23 hours, then verify rotation triggers on the next reconcile cycle.
