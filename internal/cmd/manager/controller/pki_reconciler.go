package controller

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultWebhookPKIReconcileInterval = time.Hour

// WebhookPKIReconciler periodically reconciles webhook PKI at runtime.
// This complements the startup PKI bootstrap and prevents cert/caBundle drift
// in long-running operator processes.
type WebhookPKIReconciler struct {
	client   client.Client
	options  WebhookPKIOptions
	interval time.Duration
}

// NewWebhookPKIReconciler constructs a periodic PKI reconciler runnable.
func NewWebhookPKIReconciler(c client.Client, options WebhookPKIOptions, interval time.Duration) *WebhookPKIReconciler {
	if interval <= 0 {
		interval = defaultWebhookPKIReconcileInterval
	}
	return &WebhookPKIReconciler{
		client:   c,
		options:  options,
		interval: interval,
	}
}

// Start runs the periodic PKI reconciliation loop until the context is cancelled.
func (r *WebhookPKIReconciler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("webhook-pki-reconciler")
	logger.Info("Starting periodic webhook PKI reconciliation", "interval", r.interval.String())

	reconcileOnce := func() {
		if err := EnsureWebhookPKI(ctx, r.client, r.options); err != nil {
			logger.Error(err, "Webhook PKI periodic reconciliation failed")
		}
	}

	// Run immediately on startup, then periodically.
	reconcileOnce()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping periodic webhook PKI reconciliation")
			return nil
		case <-ticker.C:
			reconcileOnce()
		}
	}
}

// NeedLeaderElection ensures this runnable executes only on the active leader.
func (r *WebhookPKIReconciler) NeedLeaderElection() bool {
	return true
}
