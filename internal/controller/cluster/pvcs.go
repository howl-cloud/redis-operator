package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// reconcilePVCs ensures each pod has a corresponding PVC.
func (r *ClusterReconciler) reconcilePVCs(ctx context.Context, cluster *redisv1.RedisCluster) (map[string]struct{}, error) {
	logger := log.FromContext(ctx)

	desired := int(cluster.Spec.Instances)
	desiredSize := cluster.Spec.Storage.Size
	if desiredSize.IsZero() {
		desiredSize = resource.MustParse("1Gi")
	}

	var healthyPVC int32
	var danglingPVC []string
	pendingResizePVCs := make(map[string]struct{})
	resizeInProgress := false
	resizeCondition := meta.FindStatusCondition(cluster.Status.Conditions, redisv1.ConditionPVCResizeInProgress)
	wasResizeInProgress := resizeCondition != nil && resizeCondition.Status == metav1.ConditionTrue

	for i := 0; i < desired; i++ {
		pvcName := pvcNameForIndex(cluster.Name, i)
		var existing corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{
			Name: pvcName, Namespace: cluster.Namespace,
		}, &existing)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("getting PVC %s: %w", pvcName, err)
			}
			if err := r.createPVC(ctx, cluster, pvcName); err != nil {
				return nil, fmt.Errorf("creating PVC %s: %w", pvcName, err)
			}
			logger.Info("Created PVC", "pvc", pvcName)
			healthyPVC++
			continue
		}

		healthyPVC++
		currentRequest := existing.Spec.Resources.Requests[corev1.ResourceStorage]
		if desiredSize.Cmp(currentRequest) > 0 {
			resizeInProgress = true

			expandable, reason, err := r.canExpandPVC(ctx, cluster, &existing)
			if err != nil {
				return nil, fmt.Errorf("checking PVC %s expansion support: %w", pvcName, err)
			}
			if !expandable {
				r.Recorder.Eventf(
					cluster,
					corev1.EventTypeWarning,
					"PVCResizeFailed",
					"Cannot resize PVC %s from %s to %s: %s",
					pvcName,
					currentRequest.String(),
					desiredSize.String(),
					reason,
				)
			} else {
				patch := client.MergeFrom(existing.DeepCopy())
				if existing.Spec.Resources.Requests == nil {
					existing.Spec.Resources.Requests = corev1.ResourceList{}
				}
				existing.Spec.Resources.Requests[corev1.ResourceStorage] = desiredSize
				if err := r.Patch(ctx, &existing, patch); err != nil {
					r.Recorder.Eventf(
						cluster,
						corev1.EventTypeWarning,
						"PVCResizeFailed",
						"Failed to patch PVC %s from %s to %s: %v",
						pvcName,
						currentRequest.String(),
						desiredSize.String(),
						err,
					)
					return nil, fmt.Errorf("patching PVC %s storage request: %w", pvcName, err)
				}
				logger.Info(
					"Patched PVC storage request",
					"pvc",
					pvcName,
					"from",
					currentRequest.String(),
					"to",
					desiredSize.String(),
				)
			}
		}

		if hasFileSystemResizePending(&existing) {
			resizeInProgress = true
			pendingResizePVCs[pvcName] = struct{}{}
		}
	}

	allPVCs, err := r.listClusterPVCs(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("listing PVCs: %w", err)
	}
	existingPods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("listing pods for PVC check: %w", err)
	}
	for _, pvc := range allPVCs {
		found := false
		for _, pod := range existingPods {
			for _, vol := range pod.Spec.Volumes {
				if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvc.Name {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			danglingPVC = append(danglingPVC, pvc.Name)
		}
	}

	patch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.HealthyPVC = healthyPVC
	cluster.Status.DanglingPVC = danglingPVC
	if resizeInProgress {
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               redisv1.ConditionPVCResizeInProgress,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Reason:             "PVCResizePending",
			Message:            "One or more PVCs are being resized or waiting for filesystem expansion",
		})
	} else if wasResizeInProgress {
		setCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               redisv1.ConditionPVCResizeInProgress,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Now(),
			Reason:             "PVCResizeComplete",
			Message:            "All PVCs are at the requested storage size",
		})
	}
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		return nil, fmt.Errorf("patching PVC status: %w", err)
	}

	if resizeInProgress && !wasResizeInProgress {
		r.Recorder.Eventf(
			cluster,
			corev1.EventTypeNormal,
			"PVCResizeStarted",
			"PVC resize started; reconciling requested storage to %s",
			desiredSize.String(),
		)
	}
	if !resizeInProgress && wasResizeInProgress {
		r.Recorder.Event(
			cluster,
			corev1.EventTypeNormal,
			"PVCResizeCompleted",
			"PVC resize completed for all cluster PVCs",
		)
	}

	return pendingResizePVCs, nil
}

func (r *ClusterReconciler) canExpandPVC(ctx context.Context, cluster *redisv1.RedisCluster, pvc *corev1.PersistentVolumeClaim) (bool, string, error) {
	storageClassName := ""
	if pvc.Spec.StorageClassName != nil {
		storageClassName = *pvc.Spec.StorageClassName
	}
	if storageClassName == "" && cluster.Spec.Storage.StorageClassName != nil {
		storageClassName = *cluster.Spec.Storage.StorageClassName
	}
	if storageClassName == "" {
		// Default StorageClass is unknown here; allow and let API admission enforce.
		return true, "", nil
	}

	var storageClass storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: storageClassName}, &storageClass); err != nil {
		if errors.IsNotFound(err) {
			return false, fmt.Sprintf("StorageClass %q not found", storageClassName), nil
		}
		return false, "", err
	}

	if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
		return false, fmt.Sprintf("StorageClass %q does not allow volume expansion", storageClassName), nil
	}

	return true, "", nil
}

func hasFileSystemResizePending(pvc *corev1.PersistentVolumeClaim) bool {
	for i := range pvc.Status.Conditions {
		condition := pvc.Status.Conditions[i]
		if condition.Type == corev1.PersistentVolumeClaimFileSystemResizePending &&
			condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// createPVC creates a PVC for a Redis instance.
func (r *ClusterReconciler) createPVC(ctx context.Context, cluster *redisv1.RedisCluster, pvcName string) error {
	storageSize := cluster.Spec.Storage.Size
	if storageSize.IsZero() {
		storageSize = resource.MustParse("1Gi")
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
			StorageClassName: cluster.Spec.Storage.StorageClassName,
		},
	}

	return r.Create(ctx, pvc)
}

func (r *ClusterReconciler) listClusterPVCs(ctx context.Context, cluster *redisv1.RedisCluster) ([]corev1.PersistentVolumeClaim, error) {
	var pvcList corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		redisv1.LabelCluster: cluster.Name,
	}); err != nil {
		return nil, err
	}
	return pvcList.Items, nil
}
