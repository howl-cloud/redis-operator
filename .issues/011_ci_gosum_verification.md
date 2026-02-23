---
id: 11
title: "CI go.sum verification"
priority: p2
type: chore
labels: [ci, security]
created: 2026-02-19
updated: 2026-02-23
depends_on: [1]
completed: true
---

## Summary

The CI pipeline (issue #1) should verify that `go.sum` is up-to-date and that dependencies have not been tampered with. Without this check, a developer can commit code that uses an updated dependency without committing the corresponding `go.sum` entry, which will silently break reproducible builds.

## Acceptance Criteria

- [x] CI step runs `go mod verify` to confirm downloaded modules match `go.sum` checksums
- [x] CI step runs `go mod tidy` and fails if it produces any diff (detects uncommitted `go.mod`/`go.sum` changes)
- [x] `GONOSUMCHECK` and `GONOSUMDB` are not set in CI (use the public checksum database)
- [x] `GOFLAGS=-mod=readonly` set in CI to prevent accidental module updates during build

## Todo

- [x] Add a dedicated `Module integrity` CI job in `.github/workflows/ci.yml`
- [x] Enforce public checksum DB usage by failing CI when `GONOSUMCHECK` or `GONOSUMDB` is set
- [x] Add `go mod verify` and `go mod tidy` + `git diff --exit-code -- go.mod go.sum` checks
- [x] Set `GOFLAGS=-mod=readonly` at workflow scope
- [x] Validate module commands locally (`go mod verify`, `go mod tidy`, `git diff --exit-code -- go.mod go.sum`)

## Notes

This is a supply-chain security control, not just housekeeping. `go mod verify` checks each downloaded module against the hash in `go.sum`, catching compromised module mirrors.
