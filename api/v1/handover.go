package v1

import "strings"

// ClusterHandoverAnnotation records owner/replica pod names during coordinated
// failover. The owner's fence withdraws readiness while Redis synchronizes the
// replica; a fence without this marker still stops Redis.
const ClusterHandoverAnnotation = "redis.io/cluster-handover"

// ClusterHandoverPods returns the participants of a planned cluster handover.
func ClusterHandoverPods(cluster *RedisCluster) (string, string) {
	if cluster.Spec.Mode != ClusterModeCluster {
		return "", ""
	}
	owner, replica, ok := strings.Cut(cluster.Annotations[ClusterHandoverAnnotation], "/")
	if !ok || owner == "" || replica == "" || owner == replica || strings.Contains(replica, "/") {
		return "", ""
	}
	return owner, replica
}
