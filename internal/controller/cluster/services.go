package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// reconcileServices ensures the three Services (-leader, -replica, -any) exist.
func (r *ClusterReconciler) reconcileServices(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if err := r.ensureService(ctx, cluster, leaderServiceName(cluster.Name), map[string]string{
		redisv1.LabelCluster: cluster.Name,
		redisv1.LabelRole:    redisv1.LabelRolePrimary,
	}); err != nil {
		return fmt.Errorf("leader service: %w", err)
	}

	if err := r.ensureService(ctx, cluster, replicaServiceName(cluster.Name), map[string]string{
		redisv1.LabelCluster: cluster.Name,
		redisv1.LabelRole:    redisv1.LabelRoleReplica,
	}); err != nil {
		return fmt.Errorf("replica service: %w", err)
	}

	if err := r.ensureService(ctx, cluster, anyServiceName(cluster.Name), map[string]string{
		redisv1.LabelCluster: cluster.Name,
	}); err != nil {
		return fmt.Errorf("any service: %w", err)
	}

	// Update leader service selector to point to current primary.
	if cluster.Status.CurrentPrimary != "" {
		return r.updateLeaderServiceSelector(ctx, cluster)
	}

	return nil
}

// ensureService creates or verifies a Service exists.
func (r *ClusterReconciler) ensureService(ctx context.Context, cluster *redisv1.RedisCluster, name string, selector map[string]string) error {
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{
		Name: name, Namespace: cluster.Namespace,
	}, &existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{
				{
					Name:       "redis",
					Port:       6379,
					TargetPort: intstr.FromInt32(6379),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}

	return r.Create(ctx, svc)
}

// updateLeaderServiceSelector updates the -leader Service to point to the current primary pod.
func (r *ClusterReconciler) updateLeaderServiceSelector(ctx context.Context, cluster *redisv1.RedisCluster) error {
	svcName := leaderServiceName(cluster.Name)
	var svc corev1.Service
	if err := r.Get(ctx, types.NamespacedName{
		Name: svcName, Namespace: cluster.Namespace,
	}, &svc); err != nil {
		return fmt.Errorf("getting leader service: %w", err)
	}

	desiredSelector := map[string]string{
		redisv1.LabelCluster:  cluster.Name,
		redisv1.LabelInstance: cluster.Status.CurrentPrimary,
	}

	if svc.Spec.Selector[redisv1.LabelInstance] == cluster.Status.CurrentPrimary {
		return nil
	}

	patch := client.MergeFrom(svc.DeepCopy())
	svc.Spec.Selector = desiredSelector
	return r.Patch(ctx, &svc, patch)
}

func leaderServiceName(clusterName string) string {
	return clusterName + "-leader"
}

func replicaServiceName(clusterName string) string {
	return clusterName + "-replica"
}

func anyServiceName(clusterName string) string {
	return clusterName + "-any"
}
