//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/howl-cloud/redis-operator/internal/instance-manager/replication"
)

const (
	redisImage = "redis:7.2"
	redisPort  = "6379/tcp"
)

func requireIntegrationDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("INTEGRATION_TESTS") == "" {
		t.Skip("set INTEGRATION_TESTS=1 to run integration tests")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
}

func startRedisContainer(
	ctx context.Context,
	t *testing.T,
	customizers ...testcontainers.ContainerCustomizer,
) testcontainers.Container {
	t.Helper()

	opts := []testcontainers.ContainerCustomizer{
		testcontainers.WithExposedPorts(redisPort),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort(redisPort).WithStartupTimeout(60 * time.Second),
		),
	}
	opts = append(opts, customizers...)

	container, err := testcontainers.Run(ctx, redisImage, opts...)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	return container
}

func newRedisClient(
	ctx context.Context,
	t *testing.T,
	container testcontainers.Container,
	username, password string,
) *redis.Client {
	t.Helper()

	host, err := container.Host(ctx)
	require.NoError(t, err)

	mappedPort, err := container.MappedPort(ctx, redisPort)
	require.NoError(t, err)

	client := redis.NewClient(&redis.Options{
		Addr:     net.JoinHostPort(host, mappedPort.Port()),
		Username: username,
		Password: password,
	})
	t.Cleanup(func() {
		_ = client.Close()
	})

	return client
}

func waitForRedisReady(t *testing.T, ctx context.Context, client *redis.Client) {
	t.Helper()
	require.Eventually(t, func() bool {
		return client.Ping(ctx).Err() == nil
	}, 30*time.Second, 250*time.Millisecond)
}

func TestGetInfo_RealRedis(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	container := startRedisContainer(ctx, t)
	client := newRedisClient(ctx, t, container, "", "")
	waitForRedisReady(t, ctx, client)

	info, err := replication.GetInfo(ctx, client)
	require.NoError(t, err)

	assert.Equal(t, "master", info.Role)
	assert.Equal(t, 0, info.ConnectedReplicas)
	assert.GreaterOrEqual(t, info.MasterReplOffset, int64(0))
}

func TestPromote_RealRedis(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	network, err := tcnetwork.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = network.Remove(context.Background())
	})

	primaryContainer := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"primary"}, network),
	)
	replicaContainer := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"replica"}, network),
	)

	replicaClient := newRedisClient(ctx, t, replicaContainer, "", "")
	waitForRedisReady(t, ctx, replicaClient)

	err = replication.SetReplicaOf(ctx, replicaClient, "primary", 6379)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, infoErr := replication.GetInfo(ctx, replicaClient)
		return infoErr == nil && info.Role == "slave" && info.MasterLinkStatus == "up"
	}, 30*time.Second, 250*time.Millisecond)

	err = replication.Promote(ctx, replicaClient)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, infoErr := replication.GetInfo(ctx, replicaClient)
		return infoErr == nil && info.Role == "master"
	}, 30*time.Second, 250*time.Millisecond)

	primaryClient := newRedisClient(ctx, t, primaryContainer, "", "")
	waitForRedisReady(t, ctx, primaryClient)
	err = primaryClient.Set(ctx, "promote-check", "ok", 0).Err()
	require.NoError(t, err)
}

func TestSetReplicaOf_RealRedis(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	network, err := tcnetwork.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = network.Remove(context.Background())
	})

	primaryContainer := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"primary"}, network),
	)
	replicaContainer := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"replica"}, network),
	)

	primaryClient := newRedisClient(ctx, t, primaryContainer, "", "")
	replicaClient := newRedisClient(ctx, t, replicaContainer, "", "")
	waitForRedisReady(t, ctx, primaryClient)
	waitForRedisReady(t, ctx, replicaClient)

	err = replication.SetReplicaOf(ctx, replicaClient, "primary", 6379)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, infoErr := replication.GetInfo(ctx, replicaClient)
		return infoErr == nil && info.Role == "slave" && info.MasterLinkStatus == "up"
	}, 30*time.Second, 250*time.Millisecond)

	err = primaryClient.Set(ctx, "replication-key", "replication-value", 0).Err()
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		val, getErr := replicaClient.Get(ctx, "replication-key").Result()
		return getErr == nil && val == "replication-value"
	}, 30*time.Second, 250*time.Millisecond)
}
