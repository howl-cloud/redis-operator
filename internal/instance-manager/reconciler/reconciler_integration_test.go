//go:build integration

package reconciler

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	integrationRedisImage = "redis:7.2"
	integrationRedisPort  = "6379/tcp"
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
		testcontainers.WithExposedPorts(integrationRedisPort),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort(integrationRedisPort).WithStartupTimeout(60 * time.Second),
		),
	}
	opts = append(opts, customizers...)

	container, err := testcontainers.Run(ctx, integrationRedisImage, opts...)
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

	mappedPort, err := container.MappedPort(ctx, integrationRedisPort)
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

func writeProjectedSecret(t *testing.T, rootDir, secretName, key, value string) {
	t.Helper()
	secretDir := filepath.Join(rootDir, secretName)
	require.NoError(t, os.MkdirAll(secretDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secretDir, key), []byte(value), 0o644))
}

func TestReconcileSecrets_RequirePass_RealRedis(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	projectedDir := filepath.Join(t.TempDir(), "projected")
	t.Setenv(envProjectedSecretsDir, projectedDir)

	password := "integration-secret-password"
	writeProjectedSecret(t, projectedDir, "auth-secret", "password", password)

	container := startRedisContainer(ctx, t)
	adminClient := newRedisClient(ctx, t, container, "", "")
	waitForRedisReady(t, ctx, adminClient)

	cluster := &redisv1.RedisCluster{}
	cluster.Namespace = "default"
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}

	rec := &InstanceReconciler{redisClient: adminClient}
	err := rec.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)

	unauthenticatedClient := newRedisClient(ctx, t, container, "", "")
	err = unauthenticatedClient.Ping(ctx).Err()
	require.Error(t, err)
	assert.Contains(t, strings.ToUpper(err.Error()), "NOAUTH")

	authenticatedClient := newRedisClient(ctx, t, container, "", password)
	waitForRedisReady(t, ctx, authenticatedClient)
}

func TestReconcileSecrets_ACLLoad_RealRedis(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	projectedDir := filepath.Join(t.TempDir(), "projected")
	dataDir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	aclFilePath := filepath.Join(dataDir, "users.acl")
	initialACL := "user default on nopass ~* +@all\n"
	require.NoError(t, os.WriteFile(aclFilePath, []byte(initialACL), 0o644))

	t.Setenv(envProjectedSecretsDir, projectedDir)
	t.Setenv(envACLFilePath, aclFilePath)

	updatedACL := strings.Join([]string{
		"user default on nopass ~* +@all",
		"user integration on >integration-pass ~* +@all",
		"",
	}, "\n")
	writeProjectedSecret(t, projectedDir, "acl-secret", "acl", updatedACL)

	container := startRedisContainer(
		ctx,
		t,
		testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			hostConfig.Binds = append(hostConfig.Binds, dataDir+":/data")
		}),
		testcontainers.WithConfigModifier(func(cfg *container.Config) {
			cfg.User = strconv.Itoa(os.Getuid())
		}),
		testcontainers.WithCmd("redis-server", "--aclfile", "/data/users.acl"),
	)

	adminClient := newRedisClient(ctx, t, container, "", "")
	waitForRedisReady(t, ctx, adminClient)

	rec := &InstanceReconciler{redisClient: adminClient}
	cluster := &redisv1.RedisCluster{}
	cluster.Namespace = "default"
	cluster.Spec.ACLConfigSecret = &redisv1.LocalObjectReference{Name: "acl-secret"}

	err := rec.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)

	writtenACL, err := os.ReadFile(aclFilePath)
	require.NoError(t, err)
	assert.Equal(t, strings.TrimSpace(updatedACL), string(writtenACL))

	exitCode, outputReader, err := container.Exec(ctx, []string{
		"redis-cli",
		"--user", "integration",
		"--pass", "integration-pass",
		"PING",
	})
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)

	outputBytes, err := io.ReadAll(outputReader)
	require.NoError(t, err)
	assert.Contains(t, strings.ToUpper(string(outputBytes)), "PONG")
}
