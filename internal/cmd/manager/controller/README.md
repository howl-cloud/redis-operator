# internal/cmd/manager/controller

Wires up and starts the `ctrl.Manager` for the operator.

## Responsibilities

The `RunController` function in this package:

1. Creates a `ctrl.Manager` with leader election, metrics, and webhook server configured.
2. Registers all reconcilers (`ClusterReconciler`, `BackupReconciler`, `ScheduledBackupReconciler`).
3. Registers defaulting and validating webhooks.
4. Sets up self-managed PKI for webhook TLS (CA secret + webhook cert secret) — no cert-manager dependency.
5. Starts the manager (blocking).

## Key Files

| File | Description |
|------|-------------|
| `controller.go` | `RunController(ctx, cfg)` — main entry point |
| `pki.go` | Self-managed CA and webhook certificate rotation |
