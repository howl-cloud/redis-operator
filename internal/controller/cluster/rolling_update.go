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
