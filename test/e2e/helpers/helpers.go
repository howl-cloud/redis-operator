// Package helpers provides shared utilities for e2e tests.
package helpers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	// DefaultPollingInterval is how frequently WaitFor* helpers poll.
	DefaultPollingInterval = 250 * time.Millisecond
)

// CreateRedisCluster creates a RedisCluster in the given namespace and returns it.
func CreateRedisCluster(ctx context.Context, c client.Client, namespace, name string, spec redisv1.RedisClusterSpec) (*redisv1.RedisCluster, error) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: spec,
	}
	if err := c.Create(ctx, cluster); err != nil {
		return nil, fmt.Errorf("creating RedisCluster %s/%s: %w", namespace, name, err)
	}
	return cluster, nil
}

// DeleteRedisCluster deletes a RedisCluster and waits for it to be gone.
func DeleteRedisCluster(ctx context.Context, c client.Client, cluster *redisv1.RedisCluster) error {
	if err := c.Delete(ctx, cluster); err != nil {
		return fmt.Errorf("deleting RedisCluster %s/%s: %w", cluster.Namespace, cluster.Name, err)
	}
	return nil
}

// WaitForPhase polls until the RedisCluster reaches the desired phase or the timeout expires.
func WaitForPhase(ctx context.Context, c client.Client, cluster *redisv1.RedisCluster, phase redisv1.ClusterPhase, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, DefaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var current redisv1.RedisCluster
		if err := c.Get(ctx, types.NamespacedName{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		}, &current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return current.Status.Phase == phase, nil
	})
}

// WaitForCondition polls until the named condition is True on the RedisCluster.
func WaitForCondition(ctx context.Context, c client.Client, cluster *redisv1.RedisCluster, conditionType string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, DefaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var current redisv1.RedisCluster
		if err := c.Get(ctx, types.NamespacedName{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		}, &current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		for _, cond := range current.Status.Conditions {
			if cond.Type == conditionType && cond.Status == metav1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// WaitForPodCount polls until the number of pods with the cluster label equals count.
func WaitForPodCount(ctx context.Context, c client.Client, cluster *redisv1.RedisCluster, count int, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, DefaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var podList corev1.PodList
		if err := c.List(ctx, &podList,
			client.InNamespace(cluster.Namespace),
			client.MatchingLabels{redisv1.LabelCluster: cluster.Name},
		); err != nil {
			return false, err
		}
		return len(podList.Items) == count, nil
	})
}

// WaitForCurrentPrimaryChange polls until status.currentPrimary differs from oldPrimary.
func WaitForCurrentPrimaryChange(ctx context.Context, c client.Client, cluster *redisv1.RedisCluster, oldPrimary string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, DefaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var current redisv1.RedisCluster
		if err := c.Get(ctx, types.NamespacedName{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		}, &current); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		return current.Status.CurrentPrimary != "" && current.Status.CurrentPrimary != oldPrimary, nil
	})
}

// MakeBasicClusterSpec returns a minimal RedisClusterSpec suitable for tests.
// envtest does not run kubelets, so use a tiny storage size.
func MakeBasicClusterSpec(instances int) redisv1.RedisClusterSpec {
	enable := true
	return redisv1.RedisClusterSpec{
		Instances: int32(instances),
		Mode:      redisv1.ClusterModeStandalone,
		ImageName: "redis:7.2",
		Storage: redisv1.StorageSpec{
			Size: resource.MustParse("1Gi"),
		},
		EnablePodDisruptionBudget: &enable,
	}
}

// GetRedisCluster fetches the latest state of a RedisCluster.
func GetRedisCluster(ctx context.Context, c client.Client, namespace, name string) (*redisv1.RedisCluster, error) {
	var cluster redisv1.RedisCluster
	if err := c.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, &cluster); err != nil {
		return nil, err
	}
	return &cluster, nil
}
