package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func (r *ClusterReconciler) beginClusterHandover(ctx context.Context, cluster *redisv1.RedisCluster, owner, heir string) error {
	if cluster.Annotations[redisv1.ClusterHandoverAnnotation] != "" {
		return fmt.Errorf("another cluster handover is pending")
	}
	fenced := r.getFencedPods(cluster)
	if slices.Contains(fenced, owner) || slices.Contains(fenced, heir) {
		return fmt.Errorf("cannot hand over between hard-fenced participants %s and %s", owner, heir)
	}
	patch := client.MergeFromWithOptions(cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	data, err := json.Marshal(append(fenced, owner))
	if err != nil {
		return fmt.Errorf("encoding handover fence: %w", err)
	}
	if cluster.Annotations == nil {
		cluster.Annotations = make(map[string]string)
	}
	cluster.Annotations[redisv1.FencingAnnotationKey] = string(data)
	cluster.Annotations[redisv1.ClusterHandoverAnnotation] = owner + "/" + heir
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("fencing handover primary %s: %w", owner, err)
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "FencingSet", "Fenced %s for coordinated handover to %s", owner, heir)
	return nil
}

func (r *ClusterReconciler) finishClusterHandover(ctx context.Context, cluster *redisv1.RedisCluster, owner string) error {
	patch := client.MergeFromWithOptions(cluster.DeepCopy(), client.MergeFromWithOptimisticLock{})
	remaining := slices.DeleteFunc(r.getFencedPods(cluster), func(name string) bool { return name == owner })
	if len(remaining) == 0 {
		delete(cluster.Annotations, redisv1.FencingAnnotationKey)
	} else {
		data, err := json.Marshal(remaining)
		if err != nil {
			return fmt.Errorf("encoding remaining fences: %w", err)
		}
		cluster.Annotations[redisv1.FencingAnnotationKey] = string(data)
	}
	delete(cluster.Annotations, redisv1.ClusterHandoverAnnotation)
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("clearing handover fence: %w", err)
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "FencingCleared", "Cleared coordinated handover fence on %s", owner)
	return nil
}

func (r *ClusterReconciler) resumeClusterHandover(ctx context.Context, cluster *redisv1.RedisCluster, pods []corev1.Pod, statuses map[string]redisv1.InstanceStatus) (bool, error) {
	owner, heir := redisv1.ClusterHandoverPods(cluster)
	if owner == "" {
		return false, fmt.Errorf("invalid cluster handover annotation")
	}
	var ownerPod, heirPod *corev1.Pod
	for i := range pods {
		if pods[i].Name == owner {
			ownerPod = &pods[i]
		}
		if pods[i].Name == heir {
			heirPod = &pods[i]
		}
	}
	// Cancel a stale handover. Do not clear an emergency fence on the target.
	fenced := r.getFencedPods(cluster)
	if ownerPod == nil || heirPod == nil || !slices.Contains(fenced, owner) || slices.Contains(fenced, heir) {
		return false, r.finishClusterHandover(ctx, cluster, owner)
	}
	source, target := statuses[owner], statuses[heir]
	if !source.Connected || !target.Connected {
		return false, nil
	}
	if target.Role == "master" && len(target.SlotsServed) > 0 && source.Role == "slave" && source.PrimaryNodeID == target.NodeID && len(source.SlotsServed) == 0 {
		return false, r.finishClusterHandover(ctx, cluster, owner)
	}
	if source.Role != "master" || target.Role != "slave" || target.PrimaryNodeID != source.NodeID || target.MasterLinkStatus != "up" || heirPod.Status.PodIP == "" {
		return false, nil
	}
	// Wait for the readiness probe to see the persisted fence before promotion.
	if isPodRunningAndReady(ownerPod) {
		return false, nil
	}
	if err := postClusterJSON(ctx, &http.Client{Timeout: statusPollTimeout}, heirPod.Status.PodIP, "/v1/promote", struct{}{}); err != nil {
		return false, fmt.Errorf("promoting %s over fenced primary %s: %w", heir, owner, err)
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ShardHandover", "Requested coordinated promotion of %s over %s", heir, owner)
	return false, nil
}
