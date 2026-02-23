// Package controller provides the RunController function that wires up
// the ctrl.Manager with all reconcilers and webhooks.
package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

const (
	defaultOperatorNamespace    = "redis-operator-system"
	defaultWebhookServiceName   = "redis-operator-webhook-service"
	webhookServiceSuffix        = "-webhook-service"
	mutatingWebhookSuffix       = "-mutating-webhook"
	validatingWebhookSuffix     = "-validating-webhook"
	serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// RunController starts the controller-manager with all reconcilers and optional webhooks.
func RunController(
	ctx context.Context,
	metricsAddr, pprofBindAddr string,
	maxConcurrentReconciles int,
	enableLeaderElection, enableWebhooks bool,
) error {
	logger := log.FromContext(ctx)
	logger.Info("Starting controller-manager",
		"metrics-addr", metricsAddr,
		"pprof-addr", pprofBindAddr,
		"max-concurrent-reconciles", maxConcurrentReconciles,
		"leader-election", enableLeaderElection,
		"webhooks-enabled", enableWebhooks,
	)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(admissionregistrationv1.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))

	options := managerOptions(scheme, metricsAddr, pprofBindAddr, enableLeaderElection, enableWebhooks)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// Create event recorder.
	recorder := mgr.GetEventRecorderFor("redis-operator")

	if enableWebhooks {
		apiClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("creating direct API client for webhook PKI: %w", err)
		}

		if err := EnsureWebhookPKI(ctx, apiClient, webhookPKIOptions(recorder)); err != nil {
			return fmt.Errorf("ensuring webhook PKI: %w", err)
		}
	}

	// Register reconcilers.
	clusterReconciler := cluster.NewClusterReconciler(mgr.GetClient(), mgr.GetScheme(), recorder, maxConcurrentReconciles)
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

		apiClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("creating direct API client for periodic webhook PKI reconciliation: %w", err)
		}
		pkiRunnable := NewWebhookPKIReconciler(apiClient, webhookPKIOptions(recorder), webhookPKIReconcileInterval())
		if err := mgr.Add(pkiRunnable); err != nil {
			return fmt.Errorf("adding periodic webhook PKI reconciler: %w", err)
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

func webhookPKIOptions(recorder record.EventRecorder) WebhookPKIOptions {
	serviceName := strings.TrimSpace(os.Getenv("WEBHOOK_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = defaultWebhookServiceName
	}

	mutatingName := strings.TrimSpace(os.Getenv("MUTATING_WEBHOOK_CONFIGURATION_NAME"))
	if mutatingName == "" {
		mutatingName = webhookConfigurationName(serviceName, mutatingWebhookSuffix)
	}

	validatingName := strings.TrimSpace(os.Getenv("VALIDATING_WEBHOOK_CONFIGURATION_NAME"))
	if validatingName == "" {
		validatingName = webhookConfigurationName(serviceName, validatingWebhookSuffix)
	}

	podName := strings.TrimSpace(os.Getenv("POD_NAME"))
	if podName == "" {
		podName = strings.TrimSpace(os.Getenv("HOSTNAME"))
	}

	return WebhookPKIOptions{
		Namespace:                          operatorNamespace(),
		ServiceName:                        serviceName,
		PodName:                            podName,
		MutatingWebhookConfigurationName:   mutatingName,
		ValidatingWebhookConfigurationName: validatingName,
		EventRecorder:                      recorder,
	}
}

func webhookConfigurationName(serviceName, suffix string) string {
	prefix := strings.TrimSuffix(serviceName, webhookServiceSuffix)
	if prefix == "" {
		prefix = serviceName
	}
	return prefix + suffix
}

func operatorNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}

	namespaceBytes, err := os.ReadFile(serviceAccountNamespacePath)
	if err == nil {
		if ns := strings.TrimSpace(string(namespaceBytes)); ns != "" {
			return ns
		}
	}

	return defaultOperatorNamespace
}

func webhookPKIReconcileInterval() time.Duration {
	val := strings.TrimSpace(os.Getenv("WEBHOOK_PKI_RECONCILE_INTERVAL"))
	if val == "" {
		return defaultWebhookPKIReconcileInterval
	}

	interval, err := time.ParseDuration(val)
	if err != nil || interval <= 0 {
		return defaultWebhookPKIReconcileInterval
	}
	return interval
}

func managerOptions(
	scheme *runtime.Scheme,
	metricsAddr, pprofBindAddr string,
	enableLeaderElection, enableWebhooks bool,
) ctrl.Options {
	options := ctrl.Options{
		Scheme: scheme,
		Metrics: server.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: ":8080",
		PprofBindAddress:       pprofBindAddr,
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
