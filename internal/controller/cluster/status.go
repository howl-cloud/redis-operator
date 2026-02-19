package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// podStatusResponse mirrors the webserver.StatusResponse for deserialization.
type podStatusResponse struct {
	Role              string `json:"role"`
	ReplicationOffset int64  `json:"replicationOffset"`
	ConnectedReplicas int    `json:"connectedReplicas"`
	MasterLinkStatus  string `json:"masterLinkStatus,omitempty"`
	Connected         bool   `json:"connected"`
}

// pollInstanceStatuses calls GET /v1/status on every live pod IP.
func (r *ClusterReconciler) pollInstanceStatuses(ctx context.Context, cluster *redisv1.RedisCluster) (map[string]redisv1.InstanceStatus, error) {
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("listing pods for status poll: %w", err)
	}

	statuses := make(map[string]redisv1.InstanceStatus)
	httpClient := &http.Client{Timeout: statusPollTimeout}

	for _, pod := range pods {
		if pod.Status.PodIP == "" {
			continue
		}

		url := fmt.Sprintf("http://%s:8080/v1/status", pod.Status.PodIP)
		status, err := pollPodStatus(ctx, httpClient, url)
		if err != nil {
			log.FromContext(ctx).Error(err, "Failed to poll pod status", "pod", pod.Name, "ip", pod.Status.PodIP)
			statuses[pod.Name] = redisv1.InstanceStatus{
				Connected: false,
			}
			continue
		}

		now := metav1.Now()
		statuses[pod.Name] = redisv1.InstanceStatus{
			Role:              status.Role,
			Connected:         status.Connected,
			ReplicationOffset: status.ReplicationOffset,
			ConnectedReplicas: int32(status.ConnectedReplicas),
			MasterLinkStatus:  status.MasterLinkStatus,
			LastSeenAt:        &now,
		}
	}

	return statuses, nil
}

// pollPodStatus fetches status from a single pod's HTTP endpoint.
func pollPodStatus(ctx context.Context, httpClient *http.Client, url string) (*podStatusResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP GET %s returned %d", url, resp.StatusCode)
	}

	var status podStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding status response: %w", err)
	}

	return &status, nil
}

// updateStatus writes collected instance statuses into the cluster status.
func (r *ClusterReconciler) updateStatus(ctx context.Context, cluster *redisv1.RedisCluster, instanceStatuses map[string]redisv1.InstanceStatus) error {
	patch := client.MergeFrom(cluster.DeepCopy())

	cluster.Status.InstancesStatus = instanceStatuses

	// Count ready instances.
	var ready int32
	for _, s := range instanceStatuses {
		if s.Connected {
			ready++
		}
	}
	cluster.Status.ReadyInstances = ready
	cluster.Status.Instances = int32(len(instanceStatuses))

	// Update phase.
	oldPhase := cluster.Status.Phase
	cluster.Status.Phase = determinePhase(cluster, instanceStatuses)

	// Record event on phase transition to Healthy.
	if oldPhase != redisv1.ClusterPhaseHealthy && cluster.Status.Phase == redisv1.ClusterPhaseHealthy {
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "ClusterReady", "Cluster is healthy with all instances connected")
	}

	// Update conditions.
	cluster.Status.Conditions = determineConditions(cluster, instanceStatuses)

	return r.Status().Patch(ctx, cluster, patch)
}

// checkReachability returns true if any expected pod is unreachable (should requeue).
func (r *ClusterReconciler) checkReachability(_ context.Context, cluster *redisv1.RedisCluster, instanceStatuses map[string]redisv1.InstanceStatus) bool {
	expected := int(cluster.Spec.Instances)
	reachable := 0
	for _, s := range instanceStatuses {
		if s.Connected {
			reachable++
		}
	}
	return reachable < expected
}

// determinePhase computes the cluster phase from instance statuses.
func determinePhase(cluster *redisv1.RedisCluster, statuses map[string]redisv1.InstanceStatus) redisv1.ClusterPhase {
	if len(statuses) == 0 {
		return redisv1.ClusterPhaseCreating
	}

	allConnected := true
	primaryFound := false
	for _, s := range statuses {
		if !s.Connected {
			allConnected = false
		}
		if s.Role == "master" {
			primaryFound = true
		}
	}

	if !primaryFound {
		return redisv1.ClusterPhaseFailingOver
	}

	if allConnected && int32(len(statuses)) >= cluster.Spec.Instances {
		return redisv1.ClusterPhaseHealthy
	}

	return redisv1.ClusterPhaseDegraded
}

// determineConditions computes the conditions for the cluster.
func determineConditions(cluster *redisv1.RedisCluster, statuses map[string]redisv1.InstanceStatus) []metav1.Condition {
	now := metav1.Now()
	var conditions []metav1.Condition

	// Ready condition.
	ready := false
	for _, s := range statuses {
		if s.Role == "master" && s.Connected {
			ready = true
			break
		}
	}
	readyCondition := metav1.Condition{
		Type:               redisv1.ConditionReady,
		LastTransitionTime: now,
	}
	if ready {
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = "ClusterReady"
		readyCondition.Message = "Cluster has a reachable primary"
	} else {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = "NoPrimaryAvailable"
		readyCondition.Message = "No reachable primary instance found"
	}
	conditions = append(conditions, readyCondition)

	// PrimaryAvailable condition.
	primaryAvailable := metav1.Condition{
		Type:               redisv1.ConditionPrimaryAvailable,
		LastTransitionTime: now,
	}
	if ready {
		primaryAvailable.Status = metav1.ConditionTrue
		primaryAvailable.Reason = "PrimaryRunning"
		primaryAvailable.Message = fmt.Sprintf("Primary %s is running", cluster.Status.CurrentPrimary)
	} else {
		primaryAvailable.Status = metav1.ConditionFalse
		primaryAvailable.Reason = "PrimaryUnavailable"
		primaryAvailable.Message = "Primary is not available"
	}
	conditions = append(conditions, primaryAvailable)

	// ReplicationHealthy condition.
	replicationHealthy := metav1.Condition{
		Type:               redisv1.ConditionReplicationHealthy,
		LastTransitionTime: now,
	}
	allReplicasConnected := true
	for name, s := range statuses {
		if name == cluster.Status.CurrentPrimary {
			continue
		}
		if !s.Connected || s.MasterLinkStatus != "up" {
			allReplicasConnected = false
			break
		}
	}
	if allReplicasConnected && len(statuses) > 1 {
		replicationHealthy.Status = metav1.ConditionTrue
		replicationHealthy.Reason = "ReplicasConnected"
		replicationHealthy.Message = "All replicas are connected and replicating"
	} else {
		replicationHealthy.Status = metav1.ConditionFalse
		replicationHealthy.Reason = "ReplicasNotHealthy"
		replicationHealthy.Message = "One or more replicas are not connected"
	}
	conditions = append(conditions, replicationHealthy)

	return conditions
}
