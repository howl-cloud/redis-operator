package cluster

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const primaryUpdateApprovalMessage = `Replicas are updated. Set annotation redis.io/approve-primary-update="true" to continue.`

// rollingUpdate performs a rolling update of pods.
// Replicas are updated first (highest ordinal first), primary last via switchover.
// The returned bool indicates whether reconciliation should stop for this cycle.
func (r *ClusterReconciler) rollingUpdate(ctx context.Context, cluster *redisv1.RedisCluster, desiredHash string) (bool, error) {
	logger := log.FromContext(ctx)

	pods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return false, fmt.Errorf("listing pods for rolling update: %w", err)
	}

	var replicas []corev1.Pod
	var primary *corev1.Pod
	for i := range pods {
		if pods[i].Name == cluster.Status.CurrentPrimary {
			primary = &pods[i]
		} else {
			replicas = append(replicas, pods[i])
		}
	}

	r.Recorder.Event(cluster, corev1.EventTypeNormal, "RollingUpdateStarted", "Rolling update started")

	// Sort replicas by ordinal descending (highest first).
	sort.Slice(replicas, func(i, j int) bool {
		return podIndex(cluster.Name, replicas[i].Name) > podIndex(cluster.Name, replicas[j].Name)
	})

	// Update replicas one at a time.
	for _, replica := range replicas {
		currentHash := getPodSpecHash(&replica)
		if currentHash == desiredHash {
			continue
		}

		logger.Info("Rolling update: deleting replica for recreate", "pod", replica.Name)
		if err := r.Delete(ctx, &replica); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("deleting replica %s for update: %w", replica.Name, err)
		}
		// Recreate will happen on next reconcile cycle.
		return true, nil // One at a time.
	}

	// Update primary last (via switchover).
	if primary != nil {
		currentHash := getPodSpecHash(primary)
		if currentHash != desiredHash {
			isSupervised := cluster.Spec.PrimaryUpdateStrategy == redisv1.PrimaryUpdateStrategySupervised
			approved := isPrimaryUpdateApproved(cluster)
			if isSupervised && !approved {
				logger.Info("Rolling update paused waiting for primary update approval", "cluster", cluster.Name)
				return r.pauseForPrimaryApproval(ctx, cluster)
			}

			if len(replicas) == 0 {
				logger.Info("Rolling update: deleting single primary for recreate", "pod", primary.Name)
				if err := r.Delete(ctx, primary); err != nil && !errors.IsNotFound(err) {
					return false, fmt.Errorf("deleting primary %s for update: %w", primary.Name, err)
				}
				if isSupervised && approved {
					if err := r.clearPrimaryUpdateApproval(ctx, cluster); err != nil {
						return false, fmt.Errorf("clearing primary update approval: %w", err)
					}
				}
				return true, nil
			}

			logger.Info("Rolling update: primary needs update, performing switchover", "pod", primary.Name)
			if err := r.switchover(ctx, cluster); err != nil {
				return false, fmt.Errorf("switchover during rolling update: %w", err)
			}
			if isSupervised && approved {
				if err := r.clearPrimaryUpdateApproval(ctx, cluster); err != nil {
					return false, fmt.Errorf("clearing primary update approval: %w", err)
				}
			}
			// After switchover, the old primary becomes a replica and will be updated
			// on the next reconcile cycle.
			return true, nil
		}
	}

	r.Recorder.Event(cluster, corev1.EventTypeNormal, "RollingUpdateCompleted", "Rolling update completed")
	return false, nil
}

// restartPodsForPendingResize performs a controlled restart sequence when PVCs
// report FileSystemResizePending. Replicas restart first (highest ordinal),
// then the primary is switched over before restart when replicas exist.
func (r *ClusterReconciler) restartPodsForPendingResize(ctx context.Context, cluster *redisv1.RedisCluster, pendingPVCs map[string]struct{}) (bool, error) {
	logger := log.FromContext(ctx)

	pods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return false, fmt.Errorf("listing pods for PVC resize restart: %w", err)
	}

	var allReplicas []corev1.Pod
	var pendingReplicas []corev1.Pod
	var pendingPrimary *corev1.Pod

	for i := range pods {
		pod := pods[i]
		if pod.Name == cluster.Status.CurrentPrimary {
			if podHasPendingResizePVC(&pod, pendingPVCs) {
				pendingPrimary = &pods[i]
			}
			continue
		}

		allReplicas = append(allReplicas, pod)
		if podHasPendingResizePVC(&pod, pendingPVCs) {
			pendingReplicas = append(pendingReplicas, pod)
		}
	}

	// Restart replicas first, highest ordinal first.
	sort.Slice(pendingReplicas, func(i, j int) bool {
		return podIndex(cluster.Name, pendingReplicas[i].Name) > podIndex(cluster.Name, pendingReplicas[j].Name)
	})
	for i := range pendingReplicas {
		target := pendingReplicas[i]
		if !isPodRunningAndReady(&target) {
			logger.Info(
				"PVC resize restart: waiting for replica to become ready before restart",
				"pod",
				target.Name,
			)
			continue
		}
		logger.Info("PVC resize restart: deleting replica for filesystem expansion", "pod", target.Name)
		r.Recorder.Eventf(
			cluster,
			corev1.EventTypeNormal,
			"PVCResizeRestart",
			"Restarting replica %s to complete filesystem expansion",
			target.Name,
		)
		if err := r.Delete(ctx, &target); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("deleting replica %s for PVC resize restart: %w", target.Name, err)
		}
		return true, nil
	}
	if len(pendingReplicas) > 0 {
		// Wait for pending replicas to become ready before issuing another restart.
		return true, nil
	}

	if pendingPrimary == nil {
		return false, nil
	}
	if !isPodRunningAndReady(pendingPrimary) {
		logger.Info(
			"PVC resize restart: waiting for primary to become ready before restart",
			"pod",
			pendingPrimary.Name,
		)
		return true, nil
	}

	if len(allReplicas) == 0 {
		logger.Info("PVC resize restart: deleting single primary for filesystem expansion", "pod", pendingPrimary.Name)
		r.Recorder.Eventf(
			cluster,
			corev1.EventTypeNormal,
			"PVCResizeRestart",
			"Restarting primary %s to complete filesystem expansion",
			pendingPrimary.Name,
		)
		if err := r.Delete(ctx, pendingPrimary); err != nil && !errors.IsNotFound(err) {
			return false, fmt.Errorf("deleting primary %s for PVC resize restart: %w", pendingPrimary.Name, err)
		}
		return true, nil
	}
	readyReplicaAvailable := false
	for i := range allReplicas {
		if isPodRunningAndReady(&allReplicas[i]) {
			readyReplicaAvailable = true
			break
		}
	}
	if !readyReplicaAvailable {
		logger.Info("PVC resize restart: waiting for at least one ready replica before primary switchover")
		return true, nil
	}

	logger.Info("PVC resize restart: primary requires restart, performing switchover", "pod", pendingPrimary.Name)
	r.Recorder.Eventf(
		cluster,
		corev1.EventTypeNormal,
		"PVCResizeRestart",
		"Primary %s requires filesystem resize; performing switchover before restart",
		pendingPrimary.Name,
	)
	if err := r.switchover(ctx, cluster); err != nil {
		return false, fmt.Errorf("switchover for PVC resize restart: %w", err)
	}
	return true, nil
}

func podHasPendingResizePVC(pod *corev1.Pod, pendingPVCs map[string]struct{}) bool {
	for i := range pod.Spec.Volumes {
		volume := pod.Spec.Volumes[i]
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		if _, ok := pendingPVCs[volume.PersistentVolumeClaim.ClaimName]; ok {
			return true
		}
	}
	return false
}

func isPodRunningAndReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for i := range pod.Status.Conditions {
		condition := pod.Status.Conditions[i]
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// switchover promotes a replica and demotes the current primary.
func (r *ClusterReconciler) switchover(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// Select the best replica (lowest replication lag).
	candidate, err := r.selectFailoverCandidate(ctx, cluster)
	if err != nil {
		return fmt.Errorf("selecting switchover candidate: %w", err)
	}
	if candidate == "" {
		return fmt.Errorf("no suitable replica found for switchover")
	}

	logger.Info("Switchover: promoting replica", "candidate", candidate, "former-primary", cluster.Status.CurrentPrimary)

	// Promote the candidate via HTTP.
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods for switchover: %w", err)
	}

	for _, pod := range pods {
		if pod.Name == candidate && pod.Status.PodIP != "" {
			if err := r.promoteInstance(ctx, pod.Status.PodIP); err != nil {
				return fmt.Errorf("promoting %s: %w", candidate, err)
			}
			break
		}
	}

	return nil
}

func isPrimaryUpdateApproved(cluster *redisv1.RedisCluster) bool {
	if cluster.Annotations == nil {
		return false
	}
	return cluster.Annotations[redisv1.AnnotationApprovePrimaryUpdate] == "true"
}

func isPrimaryUpdateWaiting(cluster *redisv1.RedisCluster) bool {
	for i := range cluster.Status.Conditions {
		condition := cluster.Status.Conditions[i]
		if condition.Type == redisv1.ConditionPrimaryUpdateWaiting && condition.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ClusterReconciler) pauseForPrimaryApproval(ctx context.Context, cluster *redisv1.RedisCluster) (bool, error) {
	waitingCondition := isPrimaryUpdateWaiting(cluster)
	if waitingCondition && cluster.Status.Phase == redisv1.ClusterPhaseWaitingForUser {
		return true, nil
	}

	patch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.Phase = redisv1.ClusterPhaseWaitingForUser
	if !waitingCondition {
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               redisv1.ConditionPrimaryUpdateWaiting,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "AwaitingApproval",
			Message:            primaryUpdateApprovalMessage,
		})
	}
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		return false, fmt.Errorf("patching waiting-for-user status: %w", err)
	}

	if !waitingCondition {
		r.Recorder.Event(
			cluster,
			corev1.EventTypeNormal,
			"PrimaryUpdatePaused",
			primaryUpdateApprovalMessage,
		)
	}
	return true, nil
}

func (r *ClusterReconciler) clearPrimaryUpdateApproval(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if cluster.Annotations != nil {
		if _, ok := cluster.Annotations[redisv1.AnnotationApprovePrimaryUpdate]; ok {
			metadataPatch := client.MergeFrom(cluster.DeepCopy())
			delete(cluster.Annotations, redisv1.AnnotationApprovePrimaryUpdate)
			if len(cluster.Annotations) == 0 {
				cluster.Annotations = nil
			}
			if err := r.Patch(ctx, cluster, metadataPatch); err != nil {
				return fmt.Errorf("patching cluster annotations: %w", err)
			}
		}
	}

	statusPatch := client.MergeFrom(cluster.DeepCopy())
	setCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               redisv1.ConditionPrimaryUpdateWaiting,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "ApprovalConsumed",
		Message:            "Primary update approval consumed; rolling update resumed.",
	})
	if cluster.Status.Phase == redisv1.ClusterPhaseWaitingForUser {
		cluster.Status.Phase = redisv1.ClusterPhaseUpdating
	}
	if err := r.Status().Patch(ctx, cluster, statusPatch); err != nil {
		return fmt.Errorf("patching status after approval: %w", err)
	}

	return nil
}
