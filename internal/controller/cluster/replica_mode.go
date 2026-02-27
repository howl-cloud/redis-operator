package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	leaderServiceReplicaModeAnnotation = "redis.io/replica-mode"
)

func isReplicaModeEnabled(cluster *redisv1.RedisCluster) bool {
	return cluster != nil &&
		cluster.Spec.ReplicaMode != nil &&
		cluster.Spec.ReplicaMode.Enabled
}

func replicaModeSourceAuthSecretName(cluster *redisv1.RedisCluster) string {
	if cluster == nil || cluster.Spec.ReplicaMode == nil || cluster.Spec.ReplicaMode.Source == nil {
		return ""
	}
	return cluster.Spec.ReplicaMode.Source.AuthSecretName
}

func (r *ClusterReconciler) reconcileReplicaModePromotion(
	ctx context.Context,
	cluster *redisv1.RedisCluster,
	statuses map[string]redisv1.InstanceStatus,
) (bool, error) {
	if cluster.Spec.ReplicaMode == nil {
		return false, nil
	}
	finalizationPending := replicaModePromotionFinalizationPending(cluster)
	promotionRequested := isReplicaModeEnabled(cluster) && cluster.Spec.ReplicaMode.Promote
	if !promotionRequested && !finalizationPending {
		return false, nil
	}
	if cluster.Status.CurrentPrimary == "" {
		return false, nil
	}

	primaryStatus, ok := statuses[cluster.Status.CurrentPrimary]
	if !ok || !primaryStatus.Connected || primaryStatus.Role != "master" {
		return false, nil
	}

	if promotionRequested {
		specPatch := client.MergeFrom(cluster.DeepCopy())
		cluster.Spec.ReplicaMode.Enabled = false
		cluster.Spec.ReplicaMode.Promote = false
		if err := r.Patch(ctx, cluster, specPatch); err != nil {
			return false, fmt.Errorf("disabling replicaMode after promotion: %w", err)
		}

		if err := r.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, cluster); err != nil {
			return false, fmt.Errorf("refetching cluster after replicaMode promotion patch: %w", err)
		}
	}

	if replicaModePromotedConditionSet(cluster) {
		return true, nil
	}

	statusPatch := client.MergeFrom(cluster.DeepCopy())
	setCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               redisv1.ConditionReplicaMode,
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             "ReplicaClusterPromoted",
		Message:            fmt.Sprintf("Replica mode disabled after promoting %s to standalone primary", cluster.Status.CurrentPrimary),
	})
	if err := r.Status().Patch(ctx, cluster, statusPatch); err != nil {
		return false, fmt.Errorf("patching replica mode promotion condition: %w", err)
	}

	r.Recorder.Eventf(
		cluster,
		corev1.EventTypeNormal,
		"ReplicaClusterPromoted",
		"Replica cluster promoted to standalone primary on pod %s",
		cluster.Status.CurrentPrimary,
	)
	return true, nil
}

func replicaModePromotionFinalizationPending(cluster *redisv1.RedisCluster) bool {
	if cluster == nil || cluster.Spec.ReplicaMode == nil {
		return false
	}
	if cluster.Spec.ReplicaMode.Enabled {
		return false
	}
	return !replicaModePromotedConditionSet(cluster)
}

func replicaModePromotedConditionSet(cluster *redisv1.RedisCluster) bool {
	if cluster == nil {
		return false
	}
	for i := range cluster.Status.Conditions {
		condition := cluster.Status.Conditions[i]
		if condition.Type == redisv1.ConditionReplicaMode &&
			condition.Status == metav1.ConditionFalse &&
			condition.Reason == "ReplicaClusterPromoted" {
			return true
		}
	}
	return false
}
