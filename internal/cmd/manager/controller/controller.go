// Package controller provides the RunController function that wires up
// the ctrl.Manager with all reconcilers and webhooks.
package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/internal/controller/backup"
	"github.com/howl-cloud/redis-operator/internal/controller/cluster"
	"github.com/howl-cloud/redis-operator/webhooks"
)

const leaderElectionID = "redis-operator-leader"

// RunController starts the controller-manager with all reconcilers and optional webhooks.
func RunController(ctx context.Context, metricsAddr string, enableLeaderElection, enableWebhooks bool) error {
	logger := log.FromContext(ctx)
	logger.Info("Starting controller-manager",
		"metrics-addr", metricsAddr,
		"leader-election", enableLeaderElection,
		"webhooks-enabled", enableWebhooks,
	)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))

	options := managerOptions(scheme, metricsAddr, enableLeaderElection, enableWebhooks)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// Create event recorder.
	recorder := mgr.GetEventRecorderFor("redis-operator")

	// Register reconcilers.
	clusterReconciler := cluster.NewClusterReconciler(mgr.GetClient(), mgr.GetScheme(), recorder)
	if err := clusterReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up ClusterReconciler: %w", err)
	}

	backupReconciler := backup.NewBackupReconciler(mgr.GetClient(), mgr.GetScheme(), recorder)
	if err := backupReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up BackupReconciler: %w", err)
	}

	scheduledBackupReconciler := backup.NewScheduledBackupReconciler(mgr.GetClient(), mgr.GetScheme(), recorder)
	if err := scheduledBackupReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up ScheduledBackupReconciler: %w", err)
	}

	if enableWebhooks {
		// Register webhooks.
		defaulter := &webhooks.RedisClusterDefaulter{}
		if err := defaulter.SetupWebhookWithManager(mgr); err != nil {
			return fmt.Errorf("setting up RedisCluster defaulter webhook: %w", err)
		}

		validator := &webhooks.RedisClusterValidator{}
		if err := validator.SetupValidatingWebhookWithManager(mgr); err != nil {
			return fmt.Errorf("setting up RedisCluster validator webhook: %w", err)
		}
	} else {
		logger.Info("Webhooks are disabled; skipping webhook registration")
	}

	// Health checks.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("setting up readiness check: %w", err)
	}

	logger.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}

	return nil
}

func managerOptions(scheme *runtime.Scheme, metricsAddr string, enableLeaderElection, enableWebhooks bool) ctrl.Options {
	options := ctrl.Options{
		Scheme: scheme,
		Metrics: server.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: ":8080",
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	}
	if enableWebhooks {
		options.WebhookServer = webhook.NewServer(webhook.Options{
			Port: 9443,
		})
	}
	return options
}
