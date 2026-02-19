package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestDefault_EmptySpec(t *testing.T) {
	d := &RedisClusterDefaulter{}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: redisv1.RedisClusterSpec{},
	}

	err := d.Default(context.Background(), cluster)
	require.NoError(t, err)

	assert.Equal(t, int32(1), cluster.Spec.Instances)
	assert.Equal(t, "redis:7.2", cluster.Spec.ImageName)
	assert.Equal(t, redisv1.ClusterModeStandalone, cluster.Spec.Mode)
	assert.Equal(t, resource.MustParse("1Gi"), cluster.Spec.Storage.Size)

	require.NotNil(t, cluster.Spec.Resources.Requests)
	assert.Equal(t, resource.MustParse("128Mi"), cluster.Spec.Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("100m"), cluster.Spec.Resources.Requests[corev1.ResourceCPU])

	require.NotNil(t, cluster.Spec.EnablePodDisruptionBudget)
	assert.True(t, *cluster.Spec.EnablePodDisruptionBudget)
}

func TestDefault_PreservesExistingValues(t *testing.T) {
	d := &RedisClusterDefaulter{}
	f := false
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: 5,
			Mode:      redisv1.ClusterModeCluster,
			ImageName: "redis:7.4",
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("10Gi"),
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("1Gi"),
					corev1.ResourceCPU:    resource.MustParse("500m"),
				},
			},
			EnablePodDisruptionBudget: &f,
		},
	}

	err := d.Default(context.Background(), cluster)
	require.NoError(t, err)

	// All values should be preserved.
	assert.Equal(t, int32(5), cluster.Spec.Instances)
	assert.Equal(t, "redis:7.4", cluster.Spec.ImageName)
	assert.Equal(t, redisv1.ClusterModeCluster, cluster.Spec.Mode)
	assert.Equal(t, resource.MustParse("10Gi"), cluster.Spec.Storage.Size)
	assert.Equal(t, resource.MustParse("1Gi"), cluster.Spec.Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("500m"), cluster.Spec.Resources.Requests[corev1.ResourceCPU])
	assert.False(t, *cluster.Spec.EnablePodDisruptionBudget)
}

func TestDefault_NonRedisClusterReturnsNil(t *testing.T) {
	d := &RedisClusterDefaulter{}
	err := d.Default(context.Background(), &redisv1.RedisBackup{})
	assert.NoError(t, err)
}

func TestDefault_PartialSpec(t *testing.T) {
	d := &RedisClusterDefaulter{}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: 3,
			// ImageName, Mode, Storage, Resources, PDB all empty.
		},
	}

	err := d.Default(context.Background(), cluster)
	require.NoError(t, err)

	// Instances should stay at 3, everything else gets defaults.
	assert.Equal(t, int32(3), cluster.Spec.Instances)
	assert.Equal(t, "redis:7.2", cluster.Spec.ImageName)
	assert.Equal(t, redisv1.ClusterModeStandalone, cluster.Spec.Mode)
	assert.Equal(t, resource.MustParse("1Gi"), cluster.Spec.Storage.Size)
	require.NotNil(t, cluster.Spec.Resources.Requests)
	require.NotNil(t, cluster.Spec.EnablePodDisruptionBudget)
	assert.True(t, *cluster.Spec.EnablePodDisruptionBudget)
}
