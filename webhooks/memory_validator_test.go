package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func memInt32(v int32) *int32 { return &v }

func memQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

// clusterWithMemoryLimit returns a valid cluster with the given container memory limit.
func clusterWithMemoryLimit(limit string) *redisv1.RedisCluster {
	cluster := validCluster()
	if limit != "" {
		cluster.Spec.Resources.Limits = corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(limit),
		}
	}
	return cluster
}

func TestValidateCreate_MemoryMutualExclusive(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")
	cluster.Spec.Memory = &redisv1.MemorySpec{
		MaxMemory:        memQuantity("256Mi"),
		MaxMemoryPercent: memInt32(75),
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

func TestValidateCreate_MemoryPercentRequiresLimit(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("")
	cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemoryPercent: memInt32(75)}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires spec.resources.limits.memory")
}

func TestValidateCreate_MaxMemoryExceedsLimit(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("256Mi")
	cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemory: memQuantity("512Mi")}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must not exceed")
}

func TestValidateCreate_MaxMemoryNonPositive(t *testing.T) {
	for _, val := range []string{"0", "-1Mi"} {
		t.Run(val, func(t *testing.T) {
			v := &RedisClusterValidator{}
			cluster := clusterWithMemoryLimit("1Gi")
			cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemory: memQuantity(val)}

			_, err := v.ValidateCreate(context.Background(), cluster)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be greater than 0")
		})
	}
}

func TestValidateCreate_MaxMemoryNonPositiveWithoutLimit(t *testing.T) {
	// Without a container limit there is no "exceeds limit" path, so the
	// non-positive check must still fire on its own.
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("")
	cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemory: memQuantity("0")}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be greater than 0")
}

func TestValidateCreate_MemoryConflictsWithRawRedisKeys(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")
	cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemoryPercent: memInt32(75)}
	cluster.Spec.Redis = map[string]string{
		"MaxMemory":        "256mb",
		"maxmemory-policy": "allkeys-lru",
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configure Redis memory via spec.memory")
}

func TestValidateCreate_MemoryValidPercent(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")
	cluster.Spec.Memory = &redisv1.MemorySpec{
		MaxMemoryPercent: memInt32(75),
		MaxMemoryPolicy:  redisv1.MaxMemoryPolicyNoEviction,
	}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateCreate_WarnsWhenLimitSetWithoutMaxMemory(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "no Redis maxmemory is configured")
}

func TestValidateCreate_NoWarnWhenRawMaxMemorySet(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")
	cluster.Spec.Redis = map[string]string{"maxmemory": "512mb"}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}

func TestValidateCreate_WarnsWhenLowHeadroom(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")
	cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemoryPercent: memInt32(95)}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "headroom")
}

func TestValidateCreate_NoWarnWithHealthyHeadroom(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := clusterWithMemoryLimit("1Gi")
	cluster.Spec.Memory = &redisv1.MemorySpec{MaxMemoryPercent: memInt32(75)}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	require.NoError(t, err)
	assert.Empty(t, warnings)
}
