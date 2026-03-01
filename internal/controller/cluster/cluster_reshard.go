package cluster

import (
	"context"
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
		// Scale-up happens later in reconcilePods. Wait for next cycle.
		return true, nil
	}
	if cluster.Status.ClusterState != "ok" || cluster.Status.SlotsAssigned < 16384 {
		// Wait for bootstrap/convergence first.
		return true, nil
	}

	groupSize := int(1 + cluster.Spec.ReplicasPerShard)
	if groupSize <= 0 {
		groupSize = 1
	}

	desiredPrimaryPods := make([]string, 0, cluster.Spec.Shards)
	for shardIndex := 0; shardIndex < int(cluster.Spec.Shards); shardIndex++ {
		podIndex := shardIndex * groupSize
		if podIndex >= desiredInstances {
			break
		}
		desiredPrimaryPods = append(desiredPrimaryPods, podNameForIndex(cluster.Name, podIndex))
	}
	if len(desiredPrimaryPods) == 0 {
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

	for _, podName := range desiredPrimaryPods {
		status, ok := statuses[podName]
		if !ok || !status.Connected || status.NodeID == "" {
			if len(statuses) > desiredInstances {
				// During downscale, block pod deletion until target primaries are ready.
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
