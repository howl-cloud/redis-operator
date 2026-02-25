package cluster

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const unknownPhase = "Unknown"

var (
	controllerRuntimeAuto = promauto.With(crmetrics.Registry)

	redisClusterInstancesTotal = controllerRuntimeAuto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_cluster_instances_total",
			Help: "Redis cluster instance counts by type (desired or actual).",
		},
		[]string{"namespace", "cluster", "type"},
	)
	redisClusterPhase = controllerRuntimeAuto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_cluster_phase",
			Help: "Current Redis cluster phase as one-hot gauge labels.",
		},
		[]string{"namespace", "cluster", "phase"},
	)
	redisReconcileDurationSeconds = controllerRuntimeAuto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "redis_reconcile_duration_seconds",
			Help:    "Duration of Redis cluster reconciliation loops in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"namespace", "cluster"},
	)
	redisFailoverTotal = controllerRuntimeAuto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "redis_failover_total",
			Help: "Total number of successful Redis failovers.",
		},
		[]string{"namespace", "cluster"},
	)
)

var knownClusterPhases = []string{
	string(redisv1.ClusterPhaseCreating),
	string(redisv1.ClusterPhaseHealthy),
	string(redisv1.ClusterPhaseDegraded),
	string(redisv1.ClusterPhaseFailingOver),
	string(redisv1.ClusterPhaseScaling),
	string(redisv1.ClusterPhaseUpdating),
	string(redisv1.ClusterPhaseDeleting),
	string(redisv1.ClusterPhaseHibernating),
	unknownPhase,
}

func observeReconcileDurationMetric(cluster *redisv1.RedisCluster, duration time.Duration) {
	namespace, name, ok := clusterMetricIdentity(cluster)
	if !ok {
		return
	}
	redisReconcileDurationSeconds.WithLabelValues(namespace, name).Observe(duration.Seconds())
}

func setClusterInstancesMetrics(cluster *redisv1.RedisCluster, actual int) {
	namespace, name, ok := clusterMetricIdentity(cluster)
	if !ok {
		return
	}
	redisClusterInstancesTotal.WithLabelValues(namespace, name, "desired").Set(float64(cluster.Spec.Instances))
	redisClusterInstancesTotal.WithLabelValues(namespace, name, "actual").Set(float64(actual))
}

func setClusterPhaseMetric(cluster *redisv1.RedisCluster) {
	namespace, name, ok := clusterMetricIdentity(cluster)
	if !ok {
		return
	}

	for _, phase := range knownClusterPhases {
		redisClusterPhase.WithLabelValues(namespace, name, phase).Set(0)
	}

	phase := string(cluster.Status.Phase)
	if phase == "" {
		phase = unknownPhase
	}
	redisClusterPhase.WithLabelValues(namespace, name, phase).Set(1)
}

func incrementFailoverMetric(cluster *redisv1.RedisCluster) {
	namespace, name, ok := clusterMetricIdentity(cluster)
	if !ok {
		return
	}
	redisFailoverTotal.WithLabelValues(namespace, name).Inc()
}

func clusterMetricIdentity(cluster *redisv1.RedisCluster) (string, string, bool) {
	if cluster == nil {
		return "", "", false
	}
	if cluster.Namespace == "" || cluster.Name == "" {
		return "", "", false
	}
	return cluster.Namespace, cluster.Name, true
}
