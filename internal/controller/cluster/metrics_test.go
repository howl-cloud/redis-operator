package cluster

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestSetClusterInstancesMetrics(t *testing.T) {
	cluster := newTestCluster("metrics-instances", "default", 3)

	setClusterInstancesMetrics(cluster, 2)

	desired := testutil.ToFloat64(redisClusterInstancesTotal.WithLabelValues("default", "metrics-instances", "desired"))
	actual := testutil.ToFloat64(redisClusterInstancesTotal.WithLabelValues("default", "metrics-instances", "actual"))
	assert.Equal(t, float64(3), desired)
	assert.Equal(t, float64(2), actual)
}

func TestSetClusterPhaseMetric(t *testing.T) {
	cluster := newTestCluster("metrics-phase", "default", 1)
	cluster.Status.Phase = redisv1.ClusterPhaseHealthy

	setClusterPhaseMetric(cluster)

	healthy := testutil.ToFloat64(redisClusterPhase.WithLabelValues("default", "metrics-phase", string(redisv1.ClusterPhaseHealthy)))
	creating := testutil.ToFloat64(redisClusterPhase.WithLabelValues("default", "metrics-phase", string(redisv1.ClusterPhaseCreating)))
	assert.Equal(t, float64(1), healthy)
	assert.Equal(t, float64(0), creating)
}

func TestObserveReconcileDurationMetric(t *testing.T) {
	cluster := newTestCluster("metrics-duration", "default", 1)

	observeReconcileDurationMetric(cluster, 250*time.Millisecond)

	observer, err := redisReconcileDurationSeconds.GetMetricWithLabelValues("default", "metrics-duration")
	require.NoError(t, err)
	metricObserver, ok := observer.(prometheus.Metric)
	require.True(t, ok)

	metric := &dto.Metric{}
	err = metricObserver.Write(metric)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, metric.GetHistogram().GetSampleCount(), uint64(1))
}

func TestIncrementFailoverMetric(t *testing.T) {
	cluster := newTestCluster("metrics-failover", "default", 2)

	incrementFailoverMetric(cluster)
	incrementFailoverMetric(cluster)

	value := testutil.ToFloat64(redisFailoverTotal.WithLabelValues("default", "metrics-failover"))
	assert.Equal(t, float64(2), value)
}

func TestControllerMetricsRegisteredInControllerRuntimeRegistry(t *testing.T) {
	families, err := crmetrics.Registry.Gather()
	require.NoError(t, err)

	registered := make(map[string]struct{}, len(families))
	for _, family := range families {
		registered[family.GetName()] = struct{}{}
	}

	expectedMetrics := []string{
		"redis_cluster_instances_total",
		"redis_cluster_phase",
		"redis_reconcile_duration_seconds",
		"redis_failover_total",
	}

	for _, expectedMetric := range expectedMetrics {
		_, ok := registered[expectedMetric]
		assert.True(t, ok, "metric %s should be registered on controller-runtime registry", expectedMetric)
	}
}
