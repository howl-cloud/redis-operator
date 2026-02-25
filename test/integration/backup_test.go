//go:build integration

package integration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type persistenceMode int

const (
	persistenceRDB persistenceMode = iota
	persistenceAOF
)

func TestRDBBackupRestoreRoundTrip(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	sourceDataDir := mustTempDir(t, "redis-src-rdb-")
	restoredDataDir := mustTempDir(t, "redis-dst-rdb-")

	source := startRedisWithDataDir(ctx, t, sourceDataDir, persistenceRDB)
	sourceClient := newRedisClient(ctx, t, source, "", "")
	waitForRedisReady(t, ctx, sourceClient)

	require.NoError(t, sourceClient.Set(ctx, "rdb-roundtrip", "ok", 0).Err())
	triggerAndWaitBGSAVE(t, ctx, sourceClient)

	copyContainerFileToHost(t, ctx, source, "/data/dump.rdb", filepath.Join(restoredDataDir, "dump.rdb"))

	restored := startRedisWithDataDir(ctx, t, restoredDataDir, persistenceRDB)
	restoredClient := newRedisClient(ctx, t, restored, "", "")
	waitForRedisReady(t, ctx, restoredClient)

	require.Eventually(t, func() bool {
		value, err := restoredClient.Get(ctx, "rdb-roundtrip").Result()
		return err == nil && value == "ok"
	}, 30*time.Second, 250*time.Millisecond)
}

func TestAOFBackupRestoreRoundTrip(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	sourceDataDir := mustTempDir(t, "redis-src-aof-")
	restoredDataDir := mustTempDir(t, "redis-dst-aof-")
	archivePath := filepath.Join(mustTempDir(t, "redis-aof-archive-"), "appendonlydir.tar.gz")

	source := startRedisWithDataDir(ctx, t, sourceDataDir, persistenceAOF)
	sourceClient := newRedisClient(ctx, t, source, "", "")
	waitForRedisReady(t, ctx, sourceClient)

	require.NoError(t, sourceClient.Set(ctx, "aof-roundtrip", "ok", 0).Err())
	triggerAndWaitBGREWRITEAOF(t, ctx, sourceClient)

	createContainerTarArchive(t, ctx, source, "/tmp/appendonlydir.tar.gz", "/data/appendonlydir")
	copyContainerFileToHost(t, ctx, source, "/tmp/appendonlydir.tar.gz", archivePath)
	require.NoError(t, extractArchive(archivePath, filepath.Join(restoredDataDir, "appendonlydir")))

	restored := startRedisWithDataDir(ctx, t, restoredDataDir, persistenceAOF)
	restoredClient := newRedisClient(ctx, t, restored, "", "")
	waitForRedisReady(t, ctx, restoredClient)

	require.Eventually(t, func() bool {
		value, err := restoredClient.Get(ctx, "aof-roundtrip").Result()
		return err == nil && value == "ok"
	}, 30*time.Second, 250*time.Millisecond)
}

func startRedisWithDataDir(ctx context.Context, t *testing.T, hostDataDir string, mode persistenceMode) testcontainers.Container {
	t.Helper()

	require.NoError(t, os.MkdirAll(hostDataDir, 0o777))
	require.NoError(t, os.Chmod(hostDataDir, 0o777))

	cmd := []string{"redis-server", "--dir", "/data"}
	switch mode {
	case persistenceRDB:
		// RDB round-trip: avoid AOF files changing startup restore behavior.
		cmd = append(cmd, "--appendonly", "no")
	case persistenceAOF:
		// AOF round-trip: disable RDB autosave to keep persisted data source unambiguous.
		cmd = append(cmd, "--appendonly", "yes", "--appenddirname", "appendonlydir", "--save", "")
	default:
		t.Fatalf("unknown persistence mode: %d", mode)
	}

	container, err := testcontainers.Run(
		ctx,
		redisImage,
		testcontainers.WithExposedPorts(redisPort),
		testcontainers.WithMounts(
			testcontainers.BindMount(hostDataDir, testcontainers.ContainerMountTarget("/data")),
		),
		testcontainers.WithCmd(cmd...),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort(redisPort).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	return container
}

func triggerAndWaitBGSAVE(t *testing.T, ctx context.Context, client *redis.Client) {
	t.Helper()

	require.NoError(t, client.BgSave(ctx).Err())
	require.Eventually(t, func() bool {
		info, err := client.Info(ctx, "persistence").Result()
		if err != nil {
			return false
		}
		return strings.Contains(info, "rdb_bgsave_in_progress:0") && strings.Contains(info, "rdb_last_bgsave_status:ok")
	}, 30*time.Second, 250*time.Millisecond)
}

func triggerAndWaitBGREWRITEAOF(t *testing.T, ctx context.Context, client *redis.Client) {
	t.Helper()

	require.NoError(t, client.Do(ctx, "BGREWRITEAOF").Err())
	require.Eventually(t, func() bool {
		info, err := client.Info(ctx, "persistence").Result()
		if err != nil {
			return false
		}
		return strings.Contains(info, "aof_rewrite_in_progress:0") && strings.Contains(info, "aof_last_bgrewrite_status:ok")
	}, 30*time.Second, 250*time.Millisecond)
}

func mustTempDir(t *testing.T, pattern string) string {
	t.Helper()

	dir, err := os.MkdirTemp("", pattern)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}

func copyContainerFileToHost(t *testing.T, ctx context.Context, container testcontainers.Container, containerPath, hostPath string) {
	t.Helper()

	reader, err := container.CopyFileFromContainer(ctx, containerPath)
	require.NoError(t, err)
	defer reader.Close() //nolint:errcheck // test helper

	require.NoError(t, os.MkdirAll(filepath.Dir(hostPath), 0o755))

	file, err := os.OpenFile(hostPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	defer file.Close() //nolint:errcheck // test helper

	_, err = io.Copy(file, reader)
	require.NoError(t, err)
}

func createContainerTarArchive(t *testing.T, ctx context.Context, container testcontainers.Container, archivePath, sourceDir string) {
	t.Helper()

	exitCode, output, err := container.Exec(ctx, []string{"tar", "-czf", archivePath, "-C", sourceDir, "."})
	require.NoError(t, err)

	outputBytes, err := io.ReadAll(output)
	require.NoError(t, err)
	require.Equalf(t, 0, exitCode, "failed to create container archive: %s", strings.TrimSpace(string(outputBytes)))
}

func extractArchive(archivePath, destinationDir string) error {
	if err := os.MkdirAll(destinationDir, 0o755); err != nil {
		return err
	}

	archiveFile, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archiveFile.Close() //nolint:errcheck // test helper

	gzipReader, err := gzip.NewReader(archiveFile)
	if err != nil {
		return err
	}
	defer gzipReader.Close() //nolint:errcheck // test helper

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		targetPath := filepath.Join(destinationDir, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tarReader); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
