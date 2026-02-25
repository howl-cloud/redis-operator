package webhooks

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// RedisClusterDefaulter implements admission.CustomDefaulter for RedisCluster.
type RedisClusterDefaulter struct{}

var _ webhook.CustomDefaulter = &RedisClusterDefaulter{}

// SetupWebhookWithManager registers the defaulting webhook with the manager.
func (d *RedisClusterDefaulter) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&redisv1.RedisCluster{}).
		WithDefaulter(d).
		Complete()
}

// Default implements webhook.CustomDefaulter.
func (d *RedisClusterDefaulter) Default(_ context.Context, obj runtime.Object) error {
	cluster, ok := obj.(*redisv1.RedisCluster)
	if !ok {
		return nil
	}

	if cluster.Spec.Instances == 0 {
		cluster.Spec.Instances = 1
	}

	if cluster.Spec.ImageName == "" {
		cluster.Spec.ImageName = "redis:7.2"
	}

	if cluster.Spec.Mode == "" {
		cluster.Spec.Mode = redisv1.ClusterModeStandalone
	}

	if cluster.Spec.PrimaryUpdateStrategy == "" {
		cluster.Spec.PrimaryUpdateStrategy = redisv1.PrimaryUpdateStrategyUnsupervised
	}

	if cluster.Spec.Storage.Size.IsZero() {
		cluster.Spec.Storage.Size = resource.MustParse("1Gi")
	}

	if cluster.Spec.Resources.Requests == nil {
		cluster.Spec.Resources.Requests = corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("128Mi"),
			corev1.ResourceCPU:    resource.MustParse("100m"),
		}
	}

	if cluster.Spec.EnablePodDisruptionBudget == nil {
		t := true
		cluster.Spec.EnablePodDisruptionBudget = &t
	}

	if cluster.Spec.PrimaryIsolation == nil {
		cluster.Spec.PrimaryIsolation = &redisv1.PrimaryIsolationSpec{}
	}
	if cluster.Spec.PrimaryIsolation.Enabled == nil {
		t := true
		cluster.Spec.PrimaryIsolation.Enabled = &t
	}
	if cluster.Spec.PrimaryIsolation.APIServerTimeout == nil {
		cluster.Spec.PrimaryIsolation.APIServerTimeout = &metav1.Duration{Duration: 5 * time.Second}
	}
	if cluster.Spec.PrimaryIsolation.PeerTimeout == nil {
		cluster.Spec.PrimaryIsolation.PeerTimeout = &metav1.Duration{Duration: 5 * time.Second}
	}

	return nil
}
