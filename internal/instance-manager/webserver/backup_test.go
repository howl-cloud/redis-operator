package webserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

type fakeRedisForBackup struct {
	listener net.Listener

	mu       sync.Mutex
	commands []string
}

func newFakeRedisForBackup(t *testing.T) (*fakeRedisForBackup, *goredis.Client) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &fakeRedisForBackup{listener: ln}
	go srv.serve()

	t.Cleanup(func() { _ = ln.Close() })

	client := goredis.NewClient(&goredis.Options{
		Addr:            ln.Addr().String(),
		Protocol:        2,
		DisableIdentity: true,
	})
	t.Cleanup(func() { _ = client.Close() })

	return srv, client
}

func (s *fakeRedisForBackup) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeRedisForBackup) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for {
		args, err := readCommand(reader)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}

		cmd := strings.ToUpper(args[0])
		s.mu.Lock()
		s.commands = append(s.commands, cmd)
		s.mu.Unlock()

		switch cmd {
		case "HELLO":
			resp := "*14\r\n" +
				"$6\r\nserver\r\n$5\r\nredis\r\n" +
				"$7\r\nversion\r\n$5\r\n7.2.0\r\n" +
				"$5\r\nproto\r\n$1\r\n2\r\n" +
				"$2\r\nid\r\n$1\r\n1\r\n" +
				"$4\r\nmode\r\n$10\r\nstandalone\r\n" +
				"$4\r\nrole\r\n$6\r\nmaster\r\n" +
				"$7\r\nmodules\r\n*0\r\n"
			_, _ = conn.Write([]byte(resp))
		case "PING":
			_, _ = conn.Write([]byte("+PONG\r\n"))
		case "CLIENT":
			_, _ = conn.Write([]byte("+OK\r\n"))
		case "BGSAVE":
			_, _ = conn.Write([]byte("+Background saving started\r\n"))
		case "BGREWRITEAOF":
			_, _ = conn.Write([]byte("+Background append only file rewriting started\r\n"))
		case "INFO":
			var info string
			if len(args) > 1 && strings.ToLower(args[1]) == "persistence" {
				info = "# Persistence\r\n" +
					"rdb_bgsave_in_progress:0\r\n" +
					"rdb_last_bgsave_status:ok\r\n" +
					"aof_rewrite_in_progress:0\r\n" +
					"aof_last_bgrewrite_status:ok\r\n"
			} else {
				info = "# Server\r\nredis_version:7.2.0\r\n"
			}
			_, _ = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(info), info)
		default:
			_, _ = conn.Write([]byte("+OK\r\n"))
		}
	}
}

func (s *fakeRedisForBackup) hasCommand(command string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cmd := range s.commands {
		if cmd == strings.ToUpper(command) {
			return true
		}
	}
	return false
}

func TestHandleBackup_RDB(t *testing.T) {
	fakeRedis, redisClient := newFakeRedisForBackup(t)

	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, defaultBackupFilename), []byte("rdb-data"), 0o600))

	srv := &Server{
		redisClient: redisClient,
		dataDir:     dataDir,
		backupUploader: func(
			_ context.Context,
			backupName string,
			destination *redisv1.S3Destination,
			artifactPath string,
			artifactType redisv1.BackupArtifactType,
		) (*backupResponse, error) {
			assert.Equal(t, "backup-rdb", backupName)
			assert.Equal(t, "test-bucket", destination.Bucket)
			assert.Equal(t, redisv1.BackupArtifactTypeRDB, artifactType)
			assert.Equal(t, filepath.Join(dataDir, defaultBackupFilename), artifactPath)
			return &backupResponse{
				ArtifactType: redisv1.BackupArtifactTypeRDB,
				BackupPath:   "s3://test-bucket/backups/backup-rdb.rdb",
				ObjectKey:    "backups/backup-rdb.rdb",
				BackupSize:   8,
			}, nil
		},
	}

	requestBody, err := json.Marshal(map[string]any{
		"backupName": "backup-rdb",
		"method":     "rdb",
		"destination": map[string]any{
			"s3": map[string]any{
				"bucket": "test-bucket",
				"path":   "backups",
			},
		},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/backup", bytes.NewReader(requestBody))
	w := httptest.NewRecorder()
	srv.handleBackup(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var response backupResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&response))
	assert.Equal(t, redisv1.BackupArtifactTypeRDB, response.ArtifactType)
	assert.Equal(t, "s3://test-bucket/backups/backup-rdb.rdb", response.BackupPath)
	assert.True(t, fakeRedis.hasCommand("BGSAVE"))
}

func TestHandleBackup_AOF(t *testing.T) {
	fakeRedis, redisClient := newFakeRedisForBackup(t)

	dataDir := t.TempDir()
	appendOnlyDir := filepath.Join(dataDir, defaultAOFDirName)
	require.NoError(t, os.MkdirAll(appendOnlyDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(appendOnlyDir, defaultAOFManifestFilename), []byte("file appendonly.aof.1.incr.aof seq 1 type i"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(appendOnlyDir, "appendonly.aof.1.incr.aof"), []byte("SET k v"), 0o600))

	srv := &Server{
		redisClient: redisClient,
		dataDir:     dataDir,
		backupUploader: func(
			_ context.Context,
			backupName string,
			destination *redisv1.S3Destination,
			artifactPath string,
			artifactType redisv1.BackupArtifactType,
		) (*backupResponse, error) {
			assert.Equal(t, "backup-aof", backupName)
			assert.Equal(t, "test-bucket", destination.Bucket)
			assert.Equal(t, redisv1.BackupArtifactTypeAOFArchive, artifactType)
			assert.True(t, strings.HasSuffix(artifactPath, ".tar.gz"))
			_, err := os.Stat(artifactPath)
			require.NoError(t, err)
			return &backupResponse{
				ArtifactType: redisv1.BackupArtifactTypeAOFArchive,
				BackupPath:   "s3://test-bucket/backups/backup-aof.aof.tar.gz",
				ObjectKey:    "backups/backup-aof.aof.tar.gz",
				BackupSize:   128,
			}, nil
		},
	}

	requestBody, err := json.Marshal(map[string]any{
		"backupName": "backup-aof",
		"method":     "aof",
		"destination": map[string]any{
			"s3": map[string]any{
				"bucket": "test-bucket",
				"path":   "backups",
			},
		},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/v1/backup", bytes.NewReader(requestBody))
	w := httptest.NewRecorder()
	srv.handleBackup(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var response backupResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&response))
	assert.Equal(t, redisv1.BackupArtifactTypeAOFArchive, response.ArtifactType)
	assert.Equal(t, "s3://test-bucket/backups/backup-aof.aof.tar.gz", response.BackupPath)
	assert.True(t, fakeRedis.hasCommand("BGREWRITEAOF"))
}
