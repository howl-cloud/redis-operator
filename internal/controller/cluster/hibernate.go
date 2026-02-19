package cluster

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// isHibernationEnabled checks if the hibernation annotation is set to "on" or "true".
func isHibernationEnabled(cluster *redisv1.RedisCluster) bool {
	val, ok := cluster.Annotations[redisv1.AnnotationHibernation]
	if !ok {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(val))
	return v == "on" || v == "true"
}

// wasHibernated checks if the cluster was previously in the Hibernating phase.
func wasHibernated(cluster *redisv1.RedisCluster) bool {
	return cluster.Status.Phase == redisv1.ClusterPhaseHibernating
}

// reconcileHibernation handles the hibernate/resume logic.
// Returns true if the cluster is hibernating and the caller should skip remaining reconciliation.
func (r *ClusterReconciler) reconcileHibernation(ctx context.Context, cluster *redisv1.RedisCluster) (bool, error) {
	logger := log.FromContext(ctx)

	if isHibernationEnabled(cluster) {
		// Hibernate: delete all pods but keep PVCs.
		if cluster.Status.Phase != redisv1.ClusterPhaseHibernating {
			logger.Info("Hibernating cluster: deleting all pods")
			r.Recorder.Event(cluster, corev1.EventTypeNormal, "Hibernating", "Cluster is entering hibernation, deleting all pods")
		}

		pods, err := r.listClusterPods(ctx, cluster)
		if err != nil {
			return false, fmt.Errorf("listing pods for hibernation: %w", err)
		}
		for i := range pods {
			if err := r.Delete(ctx, &pods[i]); err != nil && !errors.IsNotFound(err) {
				return false, fmt.Errorf("deleting pod %s for hibernation: %w", pods[i].Name, err)
			}
		}

		if err := r.clearLeaderServiceSelector(ctx, cluster); err != nil {
			return false, fmt.Errorf("clearing leader service selector: %w", err)
		}

		patch := client.MergeFrom(cluster.DeepCopy())
		cluster.Status.Phase = redisv1.ClusterPhaseHibernating
		cluster.Status.ReadyInstances = 0
		cluster.Status.Instances = 0
		cluster.Status.InstancesStatus = nil

		now := metav1.Now()
		hibernatedCondition := metav1.Condition{
			Type:               redisv1.ConditionHibernated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "HibernationEnabled",
			Message:            "Cluster is hibernated, all pods deleted",
		}
		setCondition(&cluster.Status.Conditions, hibernatedCondition)

		if err := r.Status().Patch(ctx, cluster, patch); err != nil {
			return false, fmt.Errorf("patching status for hibernation: %w", err)
		}

		return true, nil
	}

	if wasHibernated(cluster) {
		logger.Info("Resuming cluster from hibernation")
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "Resuming", "Cluster is resuming from hibernation")

		patch := client.MergeFrom(cluster.DeepCopy())
		now := metav1.Now()
		hibernatedCondition := metav1.Condition{
			Type:               redisv1.ConditionHibernated,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "HibernationDisabled",
			Message:            "Cluster is resuming from hibernation",
		}
		setCondition(&cluster.Status.Conditions, hibernatedCondition)

		// Reset phase to Creating so normal reconciliation can proceed.
		cluster.Status.Phase = redisv1.ClusterPhaseCreating

		if err := r.Status().Patch(ctx, cluster, patch); err != nil {
			return false, fmt.Errorf("patching status for resume: %w", err)
		}
	}

	return false, nil
}

// clearLeaderServiceSelector removes the selector from the -leader Service
// so that no endpoints are served while hibernating.
func (r *ClusterReconciler) clearLeaderServiceSelector(ctx context.Context, cluster *redisv1.RedisCluster) error {
	svcName := leaderServiceName(cluster.Name)
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{
		Name: svcName, Namespace: cluster.Namespace,
	}, &svc); err != nil {
		if errors.IsNotFound(err) {
			return nil // Service doesn't exist yet, nothing to clear.
		}
		return fmt.Errorf("getting leader service: %w", err)
	}

	// Set selector to an impossible match to clear endpoints.
	patch := client.MergeFrom(svc.DeepCopy())
	svc.Spec.Selector = map[string]string{
		redisv1.LabelCluster:  cluster.Name,
		redisv1.LabelInstance: "none",
	}
	return r.Patch(ctx, &svc, patch)
}

// setCondition sets or updates a condition in the conditions slice.
func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == condition.Type {
			(*conditions)[i] = condition
			return
		}
	}
	*conditions = append(*conditions, condition)
}
