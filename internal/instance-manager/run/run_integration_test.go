//go:build integration

package run

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func requireIntegrationDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("INTEGRATION_TESTS") == "" {
		t.Skip("set INTEGRATION_TESTS=1 to run integration tests")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
}

func TestWriteRedisConf_StartsServer(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	dataDirPath := overrideDataDir(t)
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"maxmemory": "256mb",
				"hz":        "25",
			},
		},
	}

	err := writeRedisConf(cluster, "")
	require.NoError(t, err)

	// Allow the container's Redis user (uid 999) to write AOF/RDB files.
	require.NoError(t, os.Chmod(dataDirPath, 0777))

	container, err := testcontainers.Run(
		ctx,
		"redis:7.2",
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithMounts(
			testcontainers.BindMount(dataDirPath, testcontainers.ContainerMountTarget(dataDirPath)),
		),
		testcontainers.WithCmd("redis-server", filepath.Join(dataDirPath, "redis.conf")),
		testcontainers.WithWaitStrategy(
			wait.ForLog("Ready to accept connections").WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)

	mappedPort, err := container.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{
		Addr: net.JoinHostPort(host, mappedPort.Port()),
	})
	t.Cleanup(func() {
		_ = client.Close()
	})

	require.Eventually(t, func() bool {
		return client.Ping(ctx).Err() == nil
	}, 30*time.Second, 250*time.Millisecond)
}
