package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// podStatusResponse mirrors the webserver.StatusResponse for deserialization.
type podStatusResponse struct {
	Role              string              `json:"role"`
	ReplicationOffset int64               `json:"replicationOffset"`
	ConnectedReplicas int                 `json:"connectedReplicas"`
	MasterLinkStatus  string              `json:"masterLinkStatus,omitempty"`
	Connected         bool                `json:"connected"`
	NodeID            string              `json:"nodeID,omitempty"`
	ClusterState      string              `json:"clusterState,omitempty"`
	SlotsServed       []redisv1.SlotRange `json:"slotsServed,omitempty"`
	Epoch             int64               `json:"epoch,omitempty"`
	SlotsAssigned     int32               `json:"slotsAssigned,omitempty"`
}

// pollInstanceStatuses calls GET /v1/status on every live pod IP.
func (r *ClusterReconciler) pollInstanceStatuses(ctx context.Context, cluster *redisv1.RedisCluster) (map[string]redisv1.InstanceStatus, error) {
	pods, err := r.listDataPods(ctx, cluster)
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
			NodeID:            status.NodeID,
			SlotsServed:       status.SlotsServed,
			ClusterState:      status.ClusterState,
			CurrentEpoch:      status.Epoch,
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
	defer func() { _ = resp.Body.Close() }()

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
	existingConditions := append([]metav1.Condition(nil), cluster.Status.Conditions...)

	cluster.Status.InstancesStatus = instanceStatuses

	var ready int32
	for _, s := range instanceStatuses {
		if s.Connected {
			ready++
		}
	}
	cluster.Status.ReadyInstances = ready
	cluster.Status.Instances = int32(len(instanceStatuses))

	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		cluster.Status.ClusterState, cluster.Status.SlotsAssigned, cluster.Status.Shards = deriveClusterStatus(cluster, instanceStatuses)
		cluster.Status.CurrentPrimary = ""
		if !cluster.Status.BootstrapCompleted &&
			cluster.Status.ClusterState == "ok" &&
			cluster.Status.SlotsAssigned == 16384 {
			cluster.Status.BootstrapCompleted = true
		}
	} else {
		cluster.Status.ClusterState = ""
		cluster.Status.SlotsAssigned = 0
		cluster.Status.BootstrapCompleted = false
		cluster.Status.Shards = nil
	}

	sentinelReady, err := r.countReadySentinelPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("counting ready sentinel pods: %w", err)
	}
	cluster.Status.SentinelReadyInstances = sentinelReady

	oldPhase := cluster.Status.Phase
	cluster.Status.Phase = determinePhase(cluster, instanceStatuses)

	if oldPhase != redisv1.ClusterPhaseHealthy && cluster.Status.Phase == redisv1.ClusterPhaseHealthy {
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "ClusterReady", "Cluster is healthy with all instances connected")
	}

	cluster.Status.Conditions = determineConditions(cluster, instanceStatuses)
	cluster.Status.Conditions = preserveConditions(
		existingConditions,
		cluster.Status.Conditions,
		redisv1.ConditionHibernated,
		redisv1.ConditionMaintenanceInProgress,
		redisv1.ConditionPrimaryUpdateWaiting,
		redisv1.ConditionPVCResizeInProgress,
		redisv1.ConditionReplicaMode,
	)

	return r.Status().Patch(ctx, cluster, patch)
}

func deriveClusterStatus(
	cluster *redisv1.RedisCluster,
	statuses map[string]redisv1.InstanceStatus,
) (string, int32, map[string]redisv1.ShardStatus) {
	shards := make(map[string]redisv1.ShardStatus)
	if cluster == nil {
		return "", 0, shards
	}

	clusterState := ""
	if len(statuses) > 0 {
		clusterState = "ok"
	}

	var slotCoverage [16384]bool
	for podName, status := range statuses {
		if status.ClusterState != "" && status.ClusterState != "ok" {
			clusterState = status.ClusterState
		}

		index := podIndex(cluster.Name, podName)
		shardIndex, replicaIndex := shardReplicaFromIndex(cluster, index)
		shardName := fmt.Sprintf("s%d", shardIndex)
		shardStatus := shards[shardName]
		shardStatus.Epoch = maxInt64(shardStatus.Epoch, status.CurrentEpoch)

		isPrimary := len(status.SlotsServed) > 0 || status.Role == "master"
		if isPrimary || (replicaIndex == 0 && shardStatus.PrimaryPod == "") {
			shardStatus.PrimaryPod = podName
			shardStatus.PrimaryNodeID = status.NodeID
			shardStatus.SlotRanges = append([]redisv1.SlotRange(nil), status.SlotsServed...)
		} else {
			shardStatus.ReplicaPods = append(shardStatus.ReplicaPods, podName)
		}
		shards[shardName] = shardStatus

		for _, slotRange := range status.SlotsServed {
			start := slotRange.Start
			end := slotRange.End
			if start < 0 {
				start = 0
			}
			if end > 16383 {
				end = 16383
			}
			for slot := start; slot <= end; slot++ {
				slotCoverage[slot] = true
			}
		}
	}

	var slotsAssigned int32
	for _, covered := range slotCoverage {
		if covered {
			slotsAssigned++
		}
	}

	for name, shardStatus := range shards {
		sort.Strings(shardStatus.ReplicaPods)
		shards[name] = shardStatus
	}

	return clusterState, slotsAssigned, shards
}

func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}

func preserveConditions(existing, current []metav1.Condition, conditionTypes ...string) []metav1.Condition {
	if len(conditionTypes) == 0 {
		return current
	}

	hasType := func(conditions []metav1.Condition, conditionType string) bool {
		for i := range conditions {
			if conditions[i].Type == conditionType {
				return true
			}
		}
		return false
	}

	for _, conditionType := range conditionTypes {
		if hasType(current, conditionType) {
			continue
		}
		for i := range existing {
			if existing[i].Type == conditionType {
				current = append(current, existing[i])
				break
			}
		}
	}

	return current
}

func (r *ClusterReconciler) countReadySentinelPods(ctx context.Context, cluster *redisv1.RedisCluster) (int32, error) {
	if cluster.Spec.Mode != redisv1.ClusterModeSentinel {
		return 0, nil
	}

	pods, err := r.listSentinelPods(ctx, cluster)
	if err != nil {
		return 0, err
	}

	var ready int32
	for _, pod := range pods {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}
	return ready, nil
}

// checkReachability returns true if any expected pod is unreachable (should requeue).
func (r *ClusterReconciler) checkReachability(_ context.Context, cluster *redisv1.RedisCluster, instanceStatuses map[string]redisv1.InstanceStatus) bool {
	expected := int(cluster.Spec.DesiredDataInstances())
	if len(instanceStatuses) < expected {
		return false
	}

	reachable := 0
	for _, s := range instanceStatuses {
		if s.Connected {
			reachable++
		}
	}
	return reachable < expected
}

func shouldTriggerFailover(cluster *redisv1.RedisCluster, statuses map[string]redisv1.InstanceStatus) bool {
	if cluster == nil {
		return false
	}
	if isReplicaModeEnabled(cluster) {
		return false
	}
	if cluster.Spec.Mode == redisv1.ClusterModeSentinel {
		return false
	}
	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		return false
	}
	currentPrimary := cluster.Status.CurrentPrimary
	if currentPrimary == "" {
		return false
	}

	primaryStatus, ok := statuses[currentPrimary]
	if ok && primaryStatus.Connected {
		return false
	}

	for podName, status := range statuses {
		if podName == currentPrimary {
			continue
		}
		if status.Connected {
			return true
		}
	}
	return false
}

// determinePhase computes the cluster phase from instance statuses.
func determinePhase(cluster *redisv1.RedisCluster, statuses map[string]redisv1.InstanceStatus) redisv1.ClusterPhase {
	if len(statuses) == 0 {
		return redisv1.ClusterPhaseCreating
	}

	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		if cluster.Status.ClusterState == "ok" && cluster.Status.SlotsAssigned == 16384 &&
			int32(len(statuses)) >= cluster.Spec.DesiredDataInstances() {
			return redisv1.ClusterPhaseHealthy
		}
		if cluster.Status.SlotsAssigned == 0 || cluster.Status.ClusterState == "" {
			return redisv1.ClusterPhaseCreating
		}
		return redisv1.ClusterPhaseDegraded
	}

	if isReplicaModeEnabled(cluster) {
		allConnected := true
		allReplicating := true
		for _, s := range statuses {
			if !s.Connected {
				allConnected = false
				allReplicating = false
				continue
			}
			if s.Role != "slave" || s.MasterLinkStatus != "up" {
				allReplicating = false
			}
		}
		if allConnected && allReplicating && int32(len(statuses)) >= cluster.Spec.DesiredDataInstances() {
			return redisv1.ClusterPhaseReplicating
		}
		return redisv1.ClusterPhaseDegraded
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
		if cluster.Spec.Mode == redisv1.ClusterModeSentinel {
			return redisv1.ClusterPhaseDegraded
		}
		return redisv1.ClusterPhaseFailingOver
	}

	if allConnected && int32(len(statuses)) >= cluster.Spec.DesiredDataInstances() {
		return redisv1.ClusterPhaseHealthy
	}

	return redisv1.ClusterPhaseDegraded
}

// determineConditions computes the conditions for the cluster.
func determineConditions(cluster *redisv1.RedisCluster, statuses map[string]redisv1.InstanceStatus) []metav1.Condition {
	now := metav1.Now()
	var conditions []metav1.Condition

	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		expectedShards := int(cluster.Spec.Shards)
		primaryCount := 0
		replicationHealthyValue := true
		for _, shardStatus := range cluster.Status.Shards {
			if shardStatus.PrimaryPod != "" {
				primaryCount++
			}
			if cluster.Spec.ReplicasPerShard > 0 {
				if len(shardStatus.ReplicaPods) < int(cluster.Spec.ReplicasPerShard) {
					replicationHealthyValue = false
				}
				for _, replicaPod := range shardStatus.ReplicaPods {
					status, ok := statuses[replicaPod]
					if !ok || !status.Connected {
						replicationHealthyValue = false
						break
					}
				}
			}
		}

		ready := cluster.Status.ClusterState == "ok" && cluster.Status.SlotsAssigned == 16384 && primaryCount >= expectedShards
		readyCondition := metav1.Condition{
			Type:               redisv1.ConditionReady,
			LastTransitionTime: now,
		}
		if ready {
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Reason = "ClusterSlotsCovered"
			readyCondition.Message = "Redis Cluster is healthy with complete slot coverage"
		} else {
			readyCondition.Status = metav1.ConditionFalse
			readyCondition.Reason = "ClusterNotReady"
			readyCondition.Message = "Redis Cluster is not healthy or slot coverage is incomplete"
		}
		conditions = append(conditions, readyCondition)

		primaryAvailable := metav1.Condition{
			Type:               redisv1.ConditionPrimaryAvailable,
			LastTransitionTime: now,
		}
		if primaryCount >= expectedShards {
			primaryAvailable.Status = metav1.ConditionTrue
			primaryAvailable.Reason = "ShardsAvailable"
			primaryAvailable.Message = "Every shard has an available primary"
		} else {
			primaryAvailable.Status = metav1.ConditionFalse
			primaryAvailable.Reason = "ShardsUnavailable"
			primaryAvailable.Message = "One or more shards do not have an available primary"
		}
		conditions = append(conditions, primaryAvailable)

		replicationHealthy := metav1.Condition{
			Type:               redisv1.ConditionReplicationHealthy,
			LastTransitionTime: now,
		}
		if replicationHealthyValue {
			replicationHealthy.Status = metav1.ConditionTrue
			replicationHealthy.Reason = "ClusterReplicasHealthy"
			replicationHealthy.Message = "Shard replicas are connected"
		} else {
			replicationHealthy.Status = metav1.ConditionFalse
			replicationHealthy.Reason = "ClusterReplicasDegraded"
			replicationHealthy.Message = "One or more shard replicas are unavailable"
		}
		conditions = append(conditions, replicationHealthy)

		return conditions
	}

	if isReplicaModeEnabled(cluster) {
		leaderStatus, leaderFound := statuses[cluster.Status.CurrentPrimary]
		leaderConnected := leaderFound && leaderStatus.Connected

		readyCondition := metav1.Condition{
			Type:               redisv1.ConditionReady,
			LastTransitionTime: now,
		}
		if leaderConnected {
			readyCondition.Status = metav1.ConditionTrue
			readyCondition.Reason = "ReplicaModeReady"
			readyCondition.Message = "Replica-mode leader is reachable"
		} else {
			readyCondition.Status = metav1.ConditionFalse
			readyCondition.Reason = "ReplicaModeLeaderUnavailable"
			readyCondition.Message = "Replica-mode leader is not reachable"
		}
		conditions = append(conditions, readyCondition)

		primaryAvailable := metav1.Condition{
			Type:               redisv1.ConditionPrimaryAvailable,
			LastTransitionTime: now,
		}
		if leaderConnected {
			primaryAvailable.Status = metav1.ConditionTrue
			primaryAvailable.Reason = "ReplicaLeaderAvailable"
			primaryAvailable.Message = fmt.Sprintf("Replica leader %s is connected", cluster.Status.CurrentPrimary)
		} else {
			primaryAvailable.Status = metav1.ConditionFalse
			primaryAvailable.Reason = "ReplicaLeaderUnavailable"
			primaryAvailable.Message = "Replica leader is not available"
		}
		conditions = append(conditions, primaryAvailable)

		replicationHealthy := metav1.Condition{
			Type:               redisv1.ConditionReplicationHealthy,
			LastTransitionTime: now,
		}
		allReplicasConnected := true
		for _, s := range statuses {
			if !s.Connected || s.Role != "slave" || s.MasterLinkStatus != "up" {
				allReplicasConnected = false
				break
			}
		}
		if allReplicasConnected && len(statuses) > 0 {
			replicationHealthy.Status = metav1.ConditionTrue
			replicationHealthy.Reason = "ExternalReplicationHealthy"
			replicationHealthy.Message = "All instances are replicating from the external source"
		} else {
			replicationHealthy.Status = metav1.ConditionFalse
			replicationHealthy.Reason = "ExternalReplicationDegraded"
			replicationHealthy.Message = "One or more instances are not replicating from the external source"
		}
		conditions = append(conditions, replicationHealthy)

		replicaModeCondition := metav1.Condition{
			Type:               redisv1.ConditionReplicaMode,
			LastTransitionTime: now,
		}
		if allReplicasConnected && len(statuses) > 0 {
			replicaModeCondition.Status = metav1.ConditionTrue
			replicaModeCondition.Reason = "ReplicatingFromExternal"
			replicaModeCondition.Message = "Replica mode is enabled and external replication is healthy"
		} else {
			replicaModeCondition.Status = metav1.ConditionFalse
			replicaModeCondition.Reason = "ExternalReplicationUnhealthy"
			replicaModeCondition.Message = "Replica mode is enabled but external replication is unhealthy"
		}
		conditions = append(conditions, replicaModeCondition)

		return conditions
	}

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
