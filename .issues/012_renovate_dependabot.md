---
id: 12
title: "Automated dependency updates via Renovate or Dependabot"
priority: p2
type: chore
labels: [ci, security, dependencies]
created: 2026-02-19
updated: 2026-02-19
depends_on: [1]
---

## Summary

Dependencies (`controller-runtime`, `client-go`, `go-redis`, etc.) are pinned but have no automated update mechanism. Security patches and minor version bumps will fall behind without automation.

## Acceptance Criteria

- [ ] Renovate or Dependabot configured for Go module updates (`go.mod`)
- [ ] Helm chart dependency updates covered (if any)
- [ ] GitHub Actions workflow versions covered
- [ ] Update PRs are grouped by ecosystem (one PR for all Go updates, not one per package)
- [ ] Auto-merge enabled for patch-level updates that pass CI
- [ ] Major version updates (e.g. `controller-runtime` v0.19 → v0.20) require manual review

## Notes

Renovate is preferred over Dependabot for Go modules — it handles `go.sum` updates correctly and supports grouping. Add a `renovate.json` at the repo root with `"extends": ["config:base"]` and Go-specific grouping rules.
