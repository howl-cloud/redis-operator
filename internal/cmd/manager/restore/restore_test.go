package restore

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

type archiveEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func TestResolveArtifactType(t *testing.T) {
	tests := []struct {
		name    string
		backup  redisv1.RedisBackup
		want    redisv1.BackupArtifactType
		wantErr bool
	}{
		{
			name: "from status",
			backup: redisv1.RedisBackup{
				Status: redisv1.RedisBackupStatus{ArtifactType: redisv1.BackupArtifactTypeAOFArchive},
			},
			want: redisv1.BackupArtifactTypeAOFArchive,
		},
		{
			name: "from method rdb",
			backup: redisv1.RedisBackup{
				Spec: redisv1.RedisBackupSpec{Method: redisv1.BackupMethodRDB},
			},
			want: redisv1.BackupArtifactTypeRDB,
		},
		{
			name: "from method aof",
			backup: redisv1.RedisBackup{
				Spec: redisv1.RedisBackupSpec{Method: redisv1.BackupMethodAOF},
			},
			want: redisv1.BackupArtifactTypeAOFArchive,
		},
		{
			name: "unknown status type",
			backup: redisv1.RedisBackup{
				Status: redisv1.RedisBackupStatus{ArtifactType: "unknown"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveArtifactType(&tt.backup)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractTarGz_ValidAOFArchive(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := createArchive(t, []archiveEntry{
		{
			name: defaultAOFManifest,
			body: []byte("file appendonly.aof.1.incr.aof seq 1 type i"),
		},
		{
			name: "appendonly.aof.1.incr.aof",
			body: []byte("*1\r\n$4\r\nPING\r\n"),
		},
	})

	err := extractTarGz(archivePath, dataDir)
	require.NoError(t, err)

	manifestPath := filepath.Join(dataDir, defaultAOFDirectory, defaultAOFManifest)
	_, err = os.Stat(manifestPath)
	require.NoError(t, err)
}

func TestExtractTarGz_SetsCrossUIDFriendlyPermissions(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := createArchive(t, []archiveEntry{
		{
			name: defaultAOFManifest,
			body: []byte("file appendonly.aof.1.incr.aof seq 1 type i"),
		},
		{
			name: "appendonly.aof.1.incr.aof",
			body: []byte("*1\r\n$4\r\nPING\r\n"),
		},
	})

	err := extractTarGz(archivePath, dataDir)
	require.NoError(t, err)

	aofDir := filepath.Join(dataDir, defaultAOFDirectory)
	dirInfo, err := os.Stat(aofDir)
	require.NoError(t, err)
	assert.Equal(t, restoredAOFDirMode, dirInfo.Mode().Perm())

	manifestInfo, err := os.Stat(filepath.Join(aofDir, defaultAOFManifest))
	require.NoError(t, err)
	assert.Equal(t, restoredAOFFileMode, manifestInfo.Mode().Perm())

	incrInfo, err := os.Stat(filepath.Join(aofDir, "appendonly.aof.1.incr.aof"))
	require.NoError(t, err)
	assert.Equal(t, restoredAOFFileMode, incrInfo.Mode().Perm())
}

func TestExtractTarGz_PathTraversalRejected(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := createArchive(t, []archiveEntry{
		{
			name: defaultAOFManifest,
			body: []byte("manifest"),
		},
		{
			name: "../escape.txt",
			body: []byte("bad"),
		},
	})

	err := extractTarGz(archivePath, dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes extraction root")
}

func TestExtractTarGz_SymlinkRejected(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := createArchive(t, []archiveEntry{
		{
			name: defaultAOFManifest,
			body: []byte("manifest"),
		},
		{
			name:     "symlink",
			typeflag: tar.TypeSymlink,
			linkname: "/etc/passwd",
		},
	})

	err := extractTarGz(archivePath, dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported tar entry type")
}

func TestExtractTarGz_MissingManifestRejected(t *testing.T) {
	dataDir := t.TempDir()
	archivePath := createArchive(t, []archiveEntry{
		{
			name: "appendonly.aof.1.incr.aof",
			body: []byte("incr"),
		},
	})

	err := extractTarGz(archivePath, dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected AOF manifest")
}

func createArchive(t *testing.T, entries []archiveEntry) string {
	t.Helper()

	archiveFile, err := os.CreateTemp(t.TempDir(), "*.tar.gz")
	require.NoError(t, err)
	defer archiveFile.Close() //nolint:errcheck // test helper

	gzipWriter := gzip.NewWriter(archiveFile)
	tarWriter := tar.NewWriter(gzipWriter)

	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o600,
			Size:     int64(len(entry.body)),
			Typeflag: typeflag,
			Linkname: entry.linkname,
		}
		if typeflag == tar.TypeDir {
			header.Mode = 0o755
			header.Size = 0
		}
		require.NoError(t, tarWriter.WriteHeader(header))
		if typeflag == tar.TypeReg {
			_, err := tarWriter.Write(entry.body)
			require.NoError(t, err)
		}
	}

	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())
	require.NoError(t, archiveFile.Close())

	return archiveFile.Name()
}
