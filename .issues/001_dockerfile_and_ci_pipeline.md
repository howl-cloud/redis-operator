---
id: 1
title: "Dockerfile and CI pipeline"
priority: p0
type: infrastructure
labels: [production-readiness, ci, infrastructure]
created: 2026-02-19
updated: 2026-02-19
depends_on: []
---

## Summary

There is currently no `Dockerfile` and no CI pipeline. The operator cannot be deployed to any cluster without a built and published OCI image, and there is no automated gate on code quality or test regressions.

## Acceptance Criteria

- [ ] Multi-stage `Dockerfile` that builds both the `controller` and `instance` subcommands into a single binary and produces a minimal runtime image (distroless or alpine)
- [ ] `docker build` produces an image that passes `docker run redis-operator --help`
- [ ] CI workflow (GitHub Actions) runs on every PR and push to `main`:
  - `go build ./...`
  - `golangci-lint run`
  - `go test ./...`
  - `docker build`
- [ ] CI publishes a tagged image to a container registry (GHCR or Docker Hub) on merge to `main`
- [ ] Image tag strategy documented (e.g. `main-<sha>`, semver tags on release)

## Notes

The `cmd/manager/main.go` binary already supports both `controller` and `instance` subcommands. The init-container pattern (copy binary into shared `emptyDir`) means a single image serves both roles — the `Dockerfile` only needs one build target.
