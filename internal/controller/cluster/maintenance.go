package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func isMaintenanceInProgress(cluster *redisv1.RedisCluster) bool {
	return cluster.Spec.NodeMaintenanceWindow != nil && cluster.Spec.NodeMaintenanceWindow.InProgress
}

func maintenanceReusePVC(cluster *redisv1.RedisCluster) bool {
	if cluster.Spec.NodeMaintenanceWindow == nil || cluster.Spec.NodeMaintenanceWindow.ReusePVC == nil {
		return true
	}
	return *cluster.Spec.NodeMaintenanceWindow.ReusePVC
}

// reconcileMaintenance handles node maintenance mode.
// Returns true when maintenance is in progress.
func (r *ClusterReconciler) reconcileMaintenance(ctx context.Context, cluster *redisv1.RedisCluster) (bool, error) {
	logger := log.FromContext(ctx)
	current := meta.FindStatusCondition(cluster.Status.Conditions, redisv1.ConditionMaintenanceInProgress)

	if !isMaintenanceInProgress(cluster) {
		if current == nil || current.Status != metav1.ConditionTrue {
			return false, nil
		}

		logger.Info("Ending node maintenance window")
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "MaintenanceWindowEnded", "Node maintenance window ended, normal reconciliation resumed")

		patch := client.MergeFrom(cluster.DeepCopy())
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               redisv1.ConditionMaintenanceInProgress,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "MaintenanceDisabled",
			Message:            "Node maintenance window is not in progress",
		})
		if err := r.Status().Patch(ctx, cluster, patch); err != nil {
			return false, fmt.Errorf("patching status after maintenance end: %w", err)
		}

		return false, nil
	}

	if err := r.deletePDB(ctx, cluster); err != nil {
		return false, fmt.Errorf("deleting PDB during maintenance: %w", err)
	}

	if current != nil && current.Status == metav1.ConditionTrue {
		return true, nil
	}

	logger.Info("Starting node maintenance window", "reusePVC", maintenanceReusePVC(cluster))
	r.Recorder.Event(cluster, corev1.EventTypeNormal, "MaintenanceWindowStarted", "Node maintenance window started, pod self-healing is suspended")

	patch := client.MergeFrom(cluster.DeepCopy())
	setCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               redisv1.ConditionMaintenanceInProgress,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "MaintenanceEnabled",
		Message:            "Node maintenance window is in progress",
	})
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		return false, fmt.Errorf("patching status for maintenance start: %w", err)
	}

	return true, nil
}

// recyclePVCsForMissingPods deletes data PVCs for missing pod ordinals so
// maintenance with reusePVC=false can recreate pods on fresh storage.
func (r *ClusterReconciler) recyclePVCsForMissingPods(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if !isMaintenanceInProgress(cluster) || maintenanceReusePVC(cluster) {
		return nil
	}

	logger := log.FromContext(ctx)
	pods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing data pods for maintenance PVC recycle: %w", err)
	}

	existingPods := make(map[string]struct{}, len(pods))
	for i := range pods {
		existingPods[pods[i].Name] = struct{}{}
	}

	for i := 0; i < int(cluster.Spec.DesiredDataInstances()); i++ {
		podName := podNameForIndex(cluster.Name, i)
		if _, ok := existingPods[podName]; ok {
			continue
		}

		pvcName := pvcNameForIndex(cluster.Name, i)
		var pvc corev1.PersistentVolumeClaim
		if err := r.Get(ctx, types.NamespacedName{
			Name:      pvcName,
			Namespace: cluster.Namespace,
		}, &pvc); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("getting PVC %s for maintenance recycle: %w", pvcName, err)
		}

		if pvc.DeletionTimestamp != nil {
			continue
		}

		logger.Info("Maintenance replacement: deleting PVC for missing pod", "pod", podName, "pvc", pvcName)
		if err := r.Delete(ctx, &pvc); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting PVC %s for maintenance recycle: %w", pvcName, err)
		}
		r.Recorder.Eventf(
			cluster,
			corev1.EventTypeNormal,
			"MaintenancePVCRecycled",
			"Deleted PVC %s for maintenance replacement of missing pod %s",
			pvcName,
			podName,
		)
	}

	return nil
}
