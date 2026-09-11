package v1

import "strings"

// ClusterHandoverAnnotation is owner/heir. Redis stays up; a fence without it still stops Redis.
const ClusterHandoverAnnotation = "redis.io/cluster-handover"

// ClusterHandoverPods returns the owner and heir of a planned cluster handover.
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
