---
id: 8
title: "Operational runbooks"
priority: p1
type: docs
labels: [documentation, production-readiness]
created: 2026-02-19
updated: 2026-02-19
depends_on: [2]
---

## Summary

There are no operational runbooks. An on-call engineer encountering a degraded cluster at 3am has no documented procedures to follow. This is a blocker for handing the operator off to an operations team.

## Acceptance Criteria

- [ ] **Runbook: Manual failover** — how to force-promote a replica when the operator cannot (e.g. operator is down)
- [ ] **Runbook: Stuck reconciler** — how to identify and unblock a reconciler loop that is not making progress (check events, check logs, force re-enqueue via annotation touch)
- [ ] **Runbook: Total cluster loss** — all Pods deleted with PVCs intact; how to restore the cluster from existing PVCs
- [ ] **Runbook: PVC corruption** — one PVC is corrupted; how to rebuild a single replica from primary data
- [ ] **Runbook: Split-brain recovery** — if split-brain is suspected (two primaries), how to identify the authoritative primary, fence the stale one, and restore replication
- [ ] **Runbook: Secret rotation** — step-by-step for rotating `authSecret` without downtime
- [ ] **Runbook: Operator upgrade** — references issue #7 upgrade story
- [ ] Runbooks published in `docs/runbooks/` as Markdown files

## Notes

Runbooks should be written by someone who has run through the scenario on a real cluster (dependency on issue #2). Write-first, validate-second is acceptable for initial drafts.
