package backup

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// startTestBackupServer starts an httptest server with the given handler
// and overrides instanceManagerPort to match the server's port.
// Returns the server and the IP to pass to triggerBackup.
func startTestBackupServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	server := httptest.NewServer(handler)

	// Parse the port from the test server.
	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	oldPort := instanceManagerPort
	instanceManagerPort = port

	cleanup := func() {
		instanceManagerPort = oldPort
		server.Close()
	}

	return "127.0.0.1", cleanup
}

func TestTriggerBackup_Success(t *testing.T) {
	ip, cleanup := startTestBackupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/backup", r.URL.Path)
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		assert.Equal(t, "backup1", payload["backupName"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"artifactType":"rdb","backupPath":"s3://test-bucket/backups/backup1.rdb","backupSize":123}`))
	}))
	defer cleanup()

	result, err := triggerBackup(context.Background(), ip, testBackup("backup1", redisv1.BackupMethodRDB))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, redisv1.BackupArtifactTypeRDB, result.ArtifactType)
	assert.Equal(t, "s3://test-bucket/backups/backup1.rdb", result.BackupPath)
	assert.Equal(t, int64(123), result.BackupSize)
}

func TestTriggerBackup_ServerError(t *testing.T) {
	ip, cleanup := startTestBackupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup()

	_, err := triggerBackup(context.Background(), ip, testBackup("backup1", redisv1.BackupMethodRDB))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestTriggerBackup_ConnectionRefused(t *testing.T) {
	// Override port to something unused.
	oldPort := instanceManagerPort
	instanceManagerPort = 1 // Privileged port, nothing should be listening.
	defer func() { instanceManagerPort = oldPort }()

	_, err := triggerBackup(context.Background(), "127.0.0.1", testBackup("backup1", redisv1.BackupMethodRDB))
	require.Error(t, err)
}

func TestTriggerBackup_ContextCancelled(t *testing.T) {
	ip, cleanup := startTestBackupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := triggerBackup(ctx, ip, testBackup("backup1", redisv1.BackupMethodRDB))
	require.Error(t, err)
}

func TestTriggerBackup_NotFound(t *testing.T) {
	ip, cleanup := startTestBackupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cleanup()

	_, err := triggerBackup(context.Background(), ip, testBackup("backup1", redisv1.BackupMethodRDB))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func testBackup(name string, method redisv1.BackupMethod) *redisv1.RedisBackup {
	return &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: redisv1.RedisBackupSpec{
			Method: method,
			Destination: &redisv1.BackupDestination{
				S3: &redisv1.S3Destination{
					Bucket: "test-bucket",
					Path:   "backups",
				},
			},
		},
	}
}
