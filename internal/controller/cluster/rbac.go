package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// reconcileServiceAccount ensures the ServiceAccount exists for instance manager pods.
func (r *ClusterReconciler) reconcileServiceAccount(ctx context.Context, cluster *redisv1.RedisCluster) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName(cluster.Name),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
	}
	if err := controllerutil.SetControllerReference(cluster, sa, r.Scheme); err != nil {
		return fmt.Errorf("setting serviceaccount owner reference: %w", err)
	}

	var existing corev1.ServiceAccount
	key := types.NamespacedName{Name: sa.Name, Namespace: sa.Namespace}
	if err := r.Get(ctx, key, &existing); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("getting serviceaccount %s: %w", sa.Name, err)
		}
		if err := r.Create(ctx, sa); err != nil {
			return fmt.Errorf("creating serviceaccount %s: %w", sa.Name, err)
		}
	}
	return nil
}

// reconcileRBAC ensures the per-cluster Role and RoleBinding for instance manager.
func (r *ClusterReconciler) reconcileRBAC(ctx context.Context, cluster *redisv1.RedisCluster) error {
	name := serviceAccountName(cluster.Name)

	desiredRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Rules: instanceManagerRoleRules(),
	}
	if err := controllerutil.SetControllerReference(cluster, desiredRole, r.Scheme); err != nil {
		return fmt.Errorf("setting role owner reference: %w", err)
	}

	var existingRole rbacv1.Role
	roleKey := types.NamespacedName{Name: name, Namespace: cluster.Namespace}
	if err := r.Get(ctx, roleKey, &existingRole); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("getting role %s: %w", name, err)
		}
		if err := r.Create(ctx, desiredRole); err != nil {
			return fmt.Errorf("creating role %s: %w", name, err)
		}
	} else {
		changed := false
		if !equality.Semantic.DeepEqual(existingRole.Labels, desiredRole.Labels) {
			existingRole.Labels = desiredRole.Labels
			changed = true
		}
		if !equality.Semantic.DeepEqual(existingRole.Rules, desiredRole.Rules) {
			existingRole.Rules = desiredRole.Rules
			changed = true
		}
		if !metav1.IsControlledBy(&existingRole, cluster) {
			if err := controllerutil.SetControllerReference(cluster, &existingRole, r.Scheme); err != nil {
				return fmt.Errorf("setting role owner reference: %w", err)
			}
			changed = true
		}
		if changed {
			if err := r.Update(ctx, &existingRole); err != nil {
				return fmt.Errorf("updating role %s: %w", name, err)
			}
		}
	}

	desiredBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      name,
				Namespace: cluster.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
	}
	if err := controllerutil.SetControllerReference(cluster, desiredBinding, r.Scheme); err != nil {
		return fmt.Errorf("setting rolebinding owner reference: %w", err)
	}

	var existingBinding rbacv1.RoleBinding
	bindingKey := types.NamespacedName{Name: name, Namespace: cluster.Namespace}
	if err := r.Get(ctx, bindingKey, &existingBinding); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("getting rolebinding %s: %w", name, err)
		}
		if err := r.Create(ctx, desiredBinding); err != nil {
			return fmt.Errorf("creating rolebinding %s: %w", name, err)
		}
		return nil
	}

	// roleRef is immutable on RoleBinding; recreate when it drifts.
	if !equality.Semantic.DeepEqual(existingBinding.RoleRef, desiredBinding.RoleRef) {
		if err := r.Delete(ctx, &existingBinding); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting rolebinding %s for immutable roleRef drift: %w", name, err)
		}
		if err := r.Create(ctx, desiredBinding); err != nil {
			return fmt.Errorf("recreating rolebinding %s: %w", name, err)
		}
		return nil
	}

	changed := false
	if !equality.Semantic.DeepEqual(existingBinding.Labels, desiredBinding.Labels) {
		existingBinding.Labels = desiredBinding.Labels
		changed = true
	}
	if !equality.Semantic.DeepEqual(existingBinding.Subjects, desiredBinding.Subjects) {
		existingBinding.Subjects = desiredBinding.Subjects
		changed = true
	}
	if !metav1.IsControlledBy(&existingBinding, cluster) {
		if err := controllerutil.SetControllerReference(cluster, &existingBinding, r.Scheme); err != nil {
			return fmt.Errorf("setting rolebinding owner reference: %w", err)
		}
		changed = true
	}
	if changed {
		if err := r.Update(ctx, &existingBinding); err != nil {
			return fmt.Errorf("updating rolebinding %s: %w", name, err)
		}
	}

	return nil
}

func instanceManagerRoleRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"redis.io"},
			Resources: []string{"redisclusters"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"redis.io"},
			Resources: []string{"redisclusters/status"},
			Verbs:     []string{"get", "patch", "update"},
		},
		{
			APIGroups: []string{"redis.io"},
			Resources: []string{"redisbackups"},
			Verbs:     []string{"get"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"events"},
			Verbs:     []string{"create", "patch"},
		},
	}
}
