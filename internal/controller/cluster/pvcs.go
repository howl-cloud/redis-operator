package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// reconcilePVCs ensures each pod has a corresponding PVC.
func (r *ClusterReconciler) reconcilePVCs(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	desired := int(cluster.Spec.Instances)

	var healthyPVC int32
	var danglingPVC []string

	for i := 0; i < desired; i++ {
		pvcName := pvcNameForIndex(cluster.Name, i)
		var existing corev1.PersistentVolumeClaim
		err := r.Get(ctx, types.NamespacedName{
			Name: pvcName, Namespace: cluster.Namespace,
		}, &existing)
		if err != nil {
			if !errors.IsNotFound(err) {
				return fmt.Errorf("getting PVC %s: %w", pvcName, err)
			}
			if err := r.createPVC(ctx, cluster, pvcName); err != nil {
				return fmt.Errorf("creating PVC %s: %w", pvcName, err)
			}
			logger.Info("Created PVC", "pvc", pvcName)
			healthyPVC++
		} else {
			healthyPVC++
		}
	}

	// Detect dangling PVCs (PVCs that exist but have no corresponding pod).
	allPVCs, err := r.listClusterPVCs(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing PVCs: %w", err)
	}
	existingPods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods for PVC check: %w", err)
	}
	podNames := make(map[string]bool)
	for _, pod := range existingPods {
		podNames[pod.Name] = true
	}
	for _, pvc := range allPVCs {
		// Check if any pod references this PVC.
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
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("patching PVC status: %w", err)
	}

	return nil
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
