package backup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	err := triggerBackup(context.Background(), ip)
	require.NoError(t, err)
}

func TestTriggerBackup_ServerError(t *testing.T) {
	ip, cleanup := startTestBackupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup()

	err := triggerBackup(context.Background(), ip)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestTriggerBackup_ConnectionRefused(t *testing.T) {
	// Override port to something unused.
	oldPort := instanceManagerPort
	instanceManagerPort = 1 // Privileged port, nothing should be listening.
	defer func() { instanceManagerPort = oldPort }()

	err := triggerBackup(context.Background(), "127.0.0.1")
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

	err := triggerBackup(ctx, ip)
	require.Error(t, err)
}

func TestTriggerBackup_NotFound(t *testing.T) {
	ip, cleanup := startTestBackupServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer cleanup()

	err := triggerBackup(context.Background(), ip)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}
