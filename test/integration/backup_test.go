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

func TestRDBBackupRestoreRoundTrip(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	sourceDataDir := mustTempDir(t, "redis-src-rdb-")
	restoredDataDir := mustTempDir(t, "redis-dst-rdb-")

	source := startRedisWithDataDir(ctx, t, sourceDataDir)
	sourceClient := newRedisClient(ctx, t, source, "", "")
	waitForRedisReady(t, ctx, sourceClient)

	require.NoError(t, sourceClient.Set(ctx, "rdb-roundtrip", "ok", 0).Err())
	triggerAndWaitBGSAVE(t, ctx, sourceClient)

	require.FileExists(t, filepath.Join(sourceDataDir, "dump.rdb"))
	require.NoError(t, copyFile(filepath.Join(sourceDataDir, "dump.rdb"), filepath.Join(restoredDataDir, "dump.rdb")))

	restored := startRedisWithDataDir(ctx, t, restoredDataDir)
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

	source := startRedisWithDataDir(ctx, t, sourceDataDir)
	sourceClient := newRedisClient(ctx, t, source, "", "")
	waitForRedisReady(t, ctx, sourceClient)

	require.NoError(t, sourceClient.Set(ctx, "aof-roundtrip", "ok", 0).Err())
	triggerAndWaitBGREWRITEAOF(t, ctx, sourceClient)

	appendOnlyDir := filepath.Join(sourceDataDir, "appendonlydir")
	require.FileExists(t, filepath.Join(appendOnlyDir, "appendonly.aof.manifest"))

	require.NoError(t, archiveDirectory(appendOnlyDir, archivePath))
	require.NoError(t, extractArchive(archivePath, filepath.Join(restoredDataDir, "appendonlydir")))

	restored := startRedisWithDataDir(ctx, t, restoredDataDir)
	restoredClient := newRedisClient(ctx, t, restored, "", "")
	waitForRedisReady(t, ctx, restoredClient)

	require.Eventually(t, func() bool {
		value, err := restoredClient.Get(ctx, "aof-roundtrip").Result()
		return err == nil && value == "ok"
	}, 30*time.Second, 250*time.Millisecond)
}

func startRedisWithDataDir(ctx context.Context, t *testing.T, hostDataDir string) testcontainers.Container {
	t.Helper()

	require.NoError(t, os.MkdirAll(hostDataDir, 0o777))
	require.NoError(t, os.Chmod(hostDataDir, 0o777))

	container, err := testcontainers.Run(
		ctx,
		redisImage,
		testcontainers.WithExposedPorts(redisPort),
		testcontainers.WithMounts(
			testcontainers.BindMount(hostDataDir, testcontainers.ContainerMountTarget("/data")),
		),
		testcontainers.WithCmd(
			"redis-server",
			"--dir", "/data",
			"--appendonly", "yes",
			"--appenddirname", "appendonlydir",
			"--save", "",
		),
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

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // test helper

	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close() //nolint:errcheck // test helper

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

func archiveDirectory(sourceDir, archivePath string) error {
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer archiveFile.Close() //nolint:errcheck // test helper

	gzipWriter := gzip.NewWriter(archiveFile)
	defer gzipWriter.Close() //nolint:errcheck // test helper

	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close() //nolint:errcheck // test helper

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceDir {
			return nil
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close() //nolint:errcheck // test helper

		_, err = io.Copy(tarWriter, file)
		return err
	})
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
			out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
