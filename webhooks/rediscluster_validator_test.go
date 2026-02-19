package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// validCluster returns a RedisCluster that passes all validation.
func validCluster() *redisv1.RedisCluster {
	return &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: 3,
			Mode:      redisv1.ClusterModeStandalone,
			ImageName: "redis:7.2",
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
			MinSyncReplicas: 0,
			MaxSyncReplicas: 0,
		},
	}
}

func TestValidateCreate_ValidCluster(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_InstancesTooLow(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 0

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instances")
	assert.Contains(t, err.Error(), "must be at least 1")
}

func TestValidateCreate_MinSyncReplicasTooHigh(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 3
	cluster.Spec.MinSyncReplicas = 3 // exceeds instances-1 (2)

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "minSyncReplicas")
}

func TestValidateCreate_MaxSyncLessThanMin(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.MinSyncReplicas = 2
	cluster.Spec.MaxSyncReplicas = 1

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxSyncReplicas")
}

func TestValidateCreate_MultipleErrors(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 0
	cluster.Spec.MinSyncReplicas = 5
	cluster.Spec.MaxSyncReplicas = 1

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	// Should contain errors for instances, minSyncReplicas, and maxSyncReplicas.
	assert.Contains(t, err.Error(), "instances")
	assert.Contains(t, err.Error(), "minSyncReplicas")
	assert.Contains(t, err.Error(), "maxSyncReplicas")
}

func TestValidateCreate_SingleInstance(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 1
	cluster.Spec.MinSyncReplicas = 0
	cluster.Spec.MaxSyncReplicas = 0

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_WrongType(t *testing.T) {
	v := &RedisClusterValidator{}
	// Pass a non-RedisCluster object.
	_, err := v.ValidateCreate(context.Background(), &redisv1.RedisBackup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected RedisCluster")
}

func TestValidateUpdate_Valid(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Instances = 5 // scaling is allowed

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StorageSizeImmutable(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Storage.Size = resource.MustParse("10Gi")

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage")
	assert.Contains(t, err.Error(), "immutable")
}

func TestValidateUpdate_ModeImmutable(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeCluster

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mode")
	assert.Contains(t, err.Error(), "immutable")
}

func TestValidateUpdate_WrongOldType(t *testing.T) {
	v := &RedisClusterValidator{}
	_, err := v.ValidateUpdate(context.Background(), &redisv1.RedisBackup{}, validCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected RedisCluster")
}

func TestValidateUpdate_WrongNewType(t *testing.T) {
	v := &RedisClusterValidator{}
	_, err := v.ValidateUpdate(context.Background(), validCluster(), &redisv1.RedisBackup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected RedisCluster")
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	v := &RedisClusterValidator{}
	warnings, err := v.ValidateDelete(context.Background(), validCluster())
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(c *redisv1.RedisCluster)
		wantErr   bool
		errSubstr string
	}{
		{
			name:   "valid 3-instance cluster",
			modify: func(_ *redisv1.RedisCluster) {},
		},
		{
			name:      "zero instances",
			modify:    func(c *redisv1.RedisCluster) { c.Spec.Instances = 0 },
			wantErr:   true,
			errSubstr: "instances",
		},
		{
			name: "minSync equals instances-1 is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.MinSyncReplicas = 2 // instances-1 = 2
				c.Spec.MaxSyncReplicas = 2
			},
		},
		{
			name: "maxSync equals minSync is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.MinSyncReplicas = 1
				c.Spec.MaxSyncReplicas = 1
			},
		},
		{
			name: "maxSync greater than minSync is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.MinSyncReplicas = 1
				c.Spec.MaxSyncReplicas = 2
			},
		},
	}

	v := &RedisClusterValidator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := validCluster()
			tt.modify(cluster)
			_, err := v.ValidateCreate(context.Background(), cluster)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errSubstr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
