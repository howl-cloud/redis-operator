# cmd/manager

Binary entry point for redis-operator.

## Overview

This package produces a single binary that serves dual roles, selected via Cobra subcommand:

```
redis-operator controller   # Runs the Kubernetes controller-manager (operator)
redis-operator instance     # Runs the in-pod instance manager (PID 1 inside Redis pods)
```

Shipping one binary simplifies image management: the same OCI image is used for the operator `Deployment` and is injected into Redis pods via an init container.

## Subcommands

| Subcommand | Entry Point | Description |
|------------|-------------|-------------|
| `controller` | `internal/cmd/manager/controller/` | Starts `ctrl.Manager`, registers reconcilers and webhooks, enables leader election |
| `instance` | `internal/instance-manager/` | Supervises `redis-server`, runs the in-pod reconcile loop and HTTP server |

## Usage

```bash
# Run the operator (typically via the Deployment manifest)
redis-operator controller --metrics-bind-address=:8080 --leader-elect

# Run the instance manager (set as the pod's container command)
redis-operator instance --cluster-name=my-cluster --pod-name=my-cluster-1
```
