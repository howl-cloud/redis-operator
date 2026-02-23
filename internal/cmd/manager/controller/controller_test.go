package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestManagerOptions_LeaderElectionEnabled(t *testing.T) {
	scheme := runtime.NewScheme()

	options := managerOptions(scheme, ":9090", ":6060", true, true)

	assert.Equal(t, scheme, options.Scheme)
	assert.Equal(t, ":9090", options.Metrics.BindAddress)
	assert.Equal(t, ":8080", options.HealthProbeBindAddress)
	assert.Equal(t, ":6060", options.PprofBindAddress)
	assert.True(t, options.LeaderElection)
	assert.Equal(t, leaderElectionID, options.LeaderElectionID)
	assert.NotNil(t, options.WebhookServer)
}

func TestManagerOptions_LeaderElectionDisabled(t *testing.T) {
	scheme := runtime.NewScheme()

	options := managerOptions(scheme, ":18080", "", false, false)

	assert.Equal(t, scheme, options.Scheme)
	assert.Equal(t, ":18080", options.Metrics.BindAddress)
	assert.Equal(t, ":8080", options.HealthProbeBindAddress)
	assert.Empty(t, options.PprofBindAddress)
	assert.False(t, options.LeaderElection)
	assert.Equal(t, leaderElectionID, options.LeaderElectionID)
	assert.Nil(t, options.WebhookServer)
}

func TestWebhookPKIReconcileInterval_Default(t *testing.T) {
	t.Setenv("WEBHOOK_PKI_RECONCILE_INTERVAL", "")
	assert.Equal(t, defaultWebhookPKIReconcileInterval, webhookPKIReconcileInterval())
}

func TestWebhookPKIReconcileInterval_Valid(t *testing.T) {
	t.Setenv("WEBHOOK_PKI_RECONCILE_INTERVAL", "45m")
	assert.Equal(t, 45*time.Minute, webhookPKIReconcileInterval())
}

func TestWebhookPKIReconcileInterval_Invalid(t *testing.T) {
	t.Setenv("WEBHOOK_PKI_RECONCILE_INTERVAL", "not-a-duration")
	assert.Equal(t, defaultWebhookPKIReconcileInterval, webhookPKIReconcileInterval())
}

func TestWebhookPKIReconcileInterval_NonPositive(t *testing.T) {
	t.Setenv("WEBHOOK_PKI_RECONCILE_INTERVAL", "-1m")
	assert.Equal(t, defaultWebhookPKIReconcileInterval, webhookPKIReconcileInterval())
}
