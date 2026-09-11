package cluster

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

type clusterMigrateRangePayload struct {
	TargetIP     string `json:"targetIP"`
	TargetPort   int    `json:"targetPort,omitempty"`
	TargetNodeID string `json:"targetNodeID"`
	SourceNodeID string `json:"sourceNodeID"`
	Start        int32  `json:"start"`
	End          int32  `json:"end"`
	BatchSize    int32  `json:"batchSize,omitempty"`
	TimeoutMS    int    `json:"timeoutMS,omitempty"`
}

func (r *ClusterReconciler) reconcileClusterReshard(
	ctx context.Context,
	cluster *redisv1.RedisCluster,
	statuses map[string]redisv1.InstanceStatus,
) (bool, error) {
	if cluster.Spec.Mode != redisv1.ClusterModeCluster {
		return true, nil
	}

	desiredInstances := int(cluster.Spec.DesiredDataInstances())
	if len(statuses) < desiredInstances {
		return true, nil
	}
	if cluster.Status.ClusterState != "ok" || cluster.Status.SlotsAssigned < 16384 {
		return true, nil
	}

	pods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return false, fmt.Errorf("listing data pods for reshard: %w", err)
	}
	podsByName := make(map[string]corev1.Pod, len(pods))
	for i := range pods {
		podsByName[pods[i].Name] = pods[i]
	}

	layout := planShardLayout(cluster, pods, statuses)
	shardCount := desiredShardCount(cluster)
	desiredPrimaryPods := make([]string, 0, shardCount)
	for shardIndex := 0; shardIndex < shardCount; shardIndex++ {
		podName := layout.primaryOf[shardIndex]
		if podName == "" {
			return false, nil
		}
		desiredPrimaryPods = append(desiredPrimaryPods, podName)
	}

	for _, podName := range desiredPrimaryPods {
		status, ok := statuses[podName]
		if !ok || !status.Connected || status.NodeID == "" {
			if len(statuses) > desiredInstances {
				// Block scale-down until the surviving primaries are reachable.
				return false, nil
			}
			return true, nil
		}
		pod, ok := podsByName[podName]
		if !ok || pod.Status.PodIP == "" {
			if len(statuses) > desiredInstances {
				return false, nil
			}
			return true, nil
		}
	}

	var slotOwner [16384]string
	for podName, status := range statuses {
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
				slotOwner[slot] = podName
			}
		}
	}

	httpClient := &http.Client{Timeout: statusPollTimeout}
	desiredRanges := calculateClusterSlotRanges(len(desiredPrimaryPods))

	for i, targetPodName := range desiredPrimaryPods {
		targetStatus := statuses[targetPodName]
		targetPod := podsByName[targetPodName]
		slotRange := desiredRanges[i]
		slot := slotRange.Start
		for slot <= slotRange.End {
			owner := slotOwner[slot]
			segmentStart := slot
			for slot <= slotRange.End && slotOwner[slot] == owner {
				slot++
			}
			segmentEnd := slot - 1

			if owner == targetPodName {
				continue
			}
			if owner == "" {
				if err := postClusterJSON(ctx, httpClient, targetPod.Status.PodIP, "/v1/cluster/addslots", clusterAddSlotsPayload{
					Start: segmentStart,
					End:   segmentEnd,
				}); err != nil {
					return false, fmt.Errorf("adding unowned slots %d-%d to %s: %w", segmentStart, segmentEnd, targetPodName, err)
				}
				return false, nil
			}

			sourceStatus, ok := statuses[owner]
			if !ok || sourceStatus.NodeID == "" {
				return false, nil
			}
			sourcePod, ok := podsByName[owner]
			if !ok || sourcePod.Status.PodIP == "" {
				return false, nil
			}

			if podIndex(cluster.Name, owner) >= desiredInstances && len(targetStatus.SlotsServed) == 0 {
				return false, r.handOverShard(ctx, httpClient, cluster, owner, sourceStatus, targetPodName, targetPod, targetStatus)
			}

			if err := postClusterJSON(ctx, httpClient, sourcePod.Status.PodIP, "/v1/cluster/migrate-range", clusterMigrateRangePayload{
				TargetIP:     targetPod.Status.PodIP,
				TargetPort:   6379,
				TargetNodeID: targetStatus.NodeID,
				SourceNodeID: sourceStatus.NodeID,
				Start:        segmentStart,
				End:          segmentEnd,
				BatchSize:    64,
				TimeoutMS:    5000,
			}); err != nil {
				return false, fmt.Errorf(
					"migrating slots %d-%d from %s to %s: %w",
					segmentStart,
					segmentEnd,
					owner,
					targetPodName,
					err,
				)
			}
			return false, nil
		}
	}

	return true, nil
}

// handOverShard moves a whole shard from a doomed pod to a surviving one
// without copying keys. The heir replicates from the owner, then takes over
// with CLUSTER FAILOVER once its link is up.
func (r *ClusterReconciler) handOverShard(
	ctx context.Context,
	httpClient *http.Client,
	cluster *redisv1.RedisCluster,
	owner string,
	ownerStatus redisv1.InstanceStatus,
	heir string,
	heirPod corev1.Pod,
	heirStatus redisv1.InstanceStatus,
) error {
	if heirStatus.Role != "slave" || heirStatus.PrimaryNodeID != ownerStatus.NodeID {
		if err := postClusterJSON(ctx, httpClient, heirPod.Status.PodIP, "/v1/cluster/replicate", clusterReplicatePayload{
			NodeID: ownerStatus.NodeID,
		}); err != nil {
			var httpErr *clusterHTTPError
			if errors.As(err, &httpErr) && httpErr.status == http.StatusConflict {
				return nil
			}
			return fmt.Errorf("attaching %s to doomed primary %s: %w", heir, owner, err)
		}
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ShardHandover", "Replicating %s from %s before scale-down", heir, owner)
		return nil
	}
	if heirStatus.MasterLinkStatus != "up" {
		return nil
	}
	return r.beginClusterHandover(ctx, cluster, owner, heir)
}
