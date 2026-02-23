package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestManagerOptions_LeaderElectionEnabled(t *testing.T) {
	scheme := runtime.NewScheme()

	options := managerOptions(scheme, ":9090", true, true)

	assert.Equal(t, scheme, options.Scheme)
	assert.Equal(t, ":9090", options.Metrics.BindAddress)
	assert.Equal(t, ":8080", options.HealthProbeBindAddress)
	assert.True(t, options.LeaderElection)
	assert.Equal(t, leaderElectionID, options.LeaderElectionID)
	assert.NotNil(t, options.WebhookServer)
}

func TestManagerOptions_LeaderElectionDisabled(t *testing.T) {
	scheme := runtime.NewScheme()

	options := managerOptions(scheme, ":18080", false, false)

	assert.Equal(t, scheme, options.Scheme)
	assert.Equal(t, ":18080", options.Metrics.BindAddress)
	assert.Equal(t, ":8080", options.HealthProbeBindAddress)
	assert.False(t, options.LeaderElection)
	assert.Equal(t, leaderElectionID, options.LeaderElectionID)
	assert.Nil(t, options.WebhookServer)
}
