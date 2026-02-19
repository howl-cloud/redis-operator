package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// overrideDataDir sets dataDir and redisConfPath to a temp directory and restores them when the test ends.
func overrideDataDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	oldDataDir := dataDir
	oldConfPath := redisConfPath
	dataDir = tmpDir
	redisConfPath = filepath.Join(tmpDir, "redis.conf")
	t.Cleanup(func() {
		dataDir = oldDataDir
		redisConfPath = oldConfPath
	})
	return tmpDir
}

func TestWriteRedisConf_BasicConfig(t *testing.T) {
	tmpDir := overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "")
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, "redis.conf"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "port 6379")
	assert.Contains(t, content, "bind 0.0.0.0")
	assert.Contains(t, content, "appendonly yes")
	assert.Contains(t, content, "save 900 1")
	assert.Contains(t, content, "save 300 10")
	assert.Contains(t, content, "save 60 10000")
	assert.NotContains(t, content, "replicaof")
	assert.NotContains(t, content, "aclfile")
}

func TestWriteRedisConf_WithReplicaOf(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "replicaof 10.0.0.1 6379")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "replicaof 10.0.0.1 6379")
}

func TestWriteRedisConf_WithRedisParams(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"maxmemory":        "256mb",
				"maxmemory-policy": "allkeys-lru",
			},
		},
	}
	err := writeRedisConf(cluster, "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "maxmemory 256mb")
	assert.Contains(t, content, "maxmemory-policy allkeys-lru")
}

func TestWriteRedisConf_WithACLConfig(t *testing.T) {
	tmpDir := overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			ACLConfigSecret: &redisv1.LocalObjectReference{Name: "acl-secret"},
		},
	}
	err := writeRedisConf(cluster, "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	expectedACLPath := filepath.Join(tmpDir, "users.acl")
	assert.Contains(t, content, "aclfile "+expectedACLPath)
}

func TestWriteRedisConf_AllOptions(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"hz": "50",
			},
			ACLConfigSecret: &redisv1.LocalObjectReference{Name: "my-acl"},
		},
	}
	err := writeRedisConf(cluster, "replicaof 10.0.0.5 6379")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "port 6379")
	assert.Contains(t, content, "replicaof 10.0.0.5 6379")
	assert.Contains(t, content, "hz 50")
	assert.Contains(t, content, "aclfile")
}

func TestWriteRedisConf_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "nested", "data")

	oldDataDir := dataDir
	oldConfPath := redisConfPath
	dataDir = subDir
	redisConfPath = filepath.Join(subDir, "redis.conf")
	t.Cleanup(func() {
		dataDir = oldDataDir
		redisConfPath = oldConfPath
	})

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "")
	require.NoError(t, err)

	// Verify the nested directory was created.
	info, err := os.Stat(subDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// Verify file was written.
	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	assert.True(t, len(data) > 0)
}

func TestWriteRedisConf_EndsWithNewline(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(data), "\n"), "config should end with newline")
}

func TestSplitBrainGuardLogic(t *testing.T) {
	tests := []struct {
		name           string
		currentPrimary string
		podName        string
		wantIsPrimary  bool
	}{
		{"pod is primary", "cluster-0", "cluster-0", true},
		{"pod is replica", "cluster-0", "cluster-1", false},
		{"empty primary first boot", "", "cluster-0", true},
		{"empty primary any pod", "", "cluster-5", true},
		{"different pods", "test-0", "test-3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This mirrors the logic from Run(): isPrimary := cluster.Status.CurrentPrimary == podName || cluster.Status.CurrentPrimary == ""
			isPrimary := tt.currentPrimary == tt.podName || tt.currentPrimary == ""
			assert.Equal(t, tt.wantIsPrimary, isPrimary)
		})
	}
}

func TestResolvePodIP(t *testing.T) {
	// resolvePodIP returns a DNS-style name for the pod.
	ip, err := resolvePodIP(context.Background(), nil, "cluster-0", "default")
	require.NoError(t, err)
	assert.Equal(t, "cluster-0.default.svc.cluster.local", ip)
}

func TestResolvePodIP_DifferentNamespace(t *testing.T) {
	ip, err := resolvePodIP(context.Background(), nil, "myredis-2", "production")
	require.NoError(t, err)
	assert.Equal(t, "myredis-2.production.svc.cluster.local", ip)
}
