package cluster

import (
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// reconcilePDB creates or updates the PodDisruptionBudget.
func (r *ClusterReconciler) reconcilePDB(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if cluster.Spec.EnablePodDisruptionBudget != nil && !*cluster.Spec.EnablePodDisruptionBudget {
		return r.deletePDB(ctx, cluster)
	}

	pdbName := fmt.Sprintf("%s-pdb", cluster.Name)
	minAvailable := pdbMinAvailable(cluster.Spec.Instances)

	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pdbName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					redisv1.LabelCluster: cluster.Name,
				},
			},
		},
	}

	var existing policyv1.PodDisruptionBudget
	err := r.Get(ctx, types.NamespacedName{
		Name: pdbName, Namespace: cluster.Namespace,
	}, &existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return fmt.Errorf("getting PDB: %w", err)
	}

	if existing.Spec.MinAvailable == nil || existing.Spec.MinAvailable.IntValue() != minAvailable.IntValue() {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.MinAvailable = &minAvailable
		return r.Patch(ctx, &existing, patch)
	}

	return nil
}

// deletePDB removes the PDB if it exists.
func (r *ClusterReconciler) deletePDB(ctx context.Context, cluster *redisv1.RedisCluster) error {
	pdbName := fmt.Sprintf("%s-pdb", cluster.Name)
	var existing policyv1.PodDisruptionBudget
	err := r.Get(ctx, types.NamespacedName{
		Name: pdbName, Namespace: cluster.Namespace,
	}, &existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return r.Delete(ctx, &existing)
}

// pdbMinAvailable computes max(1, instances-1).
func pdbMinAvailable(instances int32) intstr.IntOrString {
	min := instances - 1
	if min < 1 {
		min = 1
	}
	return intstr.FromInt32(min)
}
