package webserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer creates a Server with mock functions and no real Redis client.
func newTestServer(
	promoteFunc func(ctx context.Context) error,
	demoteFunc func(ctx context.Context, primaryIP string, port int) error,
) *Server {
	return &Server{
		redisClient: nil, // not used in handler tests that don't call Redis
		listenAddr:  ":0",
		promoteFunc: promoteFunc,
		demoteFunc:  demoteFunc,
	}
}

func TestHandleHealthz_ProcessRunning(t *testing.T) {
	srv := newTestServer(nil, nil)

	// Create a dummy command that simulates a running process.
	// Use a long-running command that we'll never actually wait on.
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	defer cmd.Process.Kill() //nolint:errcheck

	srv.SetRedisCmd(cmd)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealthz(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestHandleHealthz_NilRedisCmd(t *testing.T) {
	srv := newTestServer(nil, nil)
	// redisCmd is nil by default.

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealthz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "redis-server not running")
}

func TestHandleStatus_WrongMethod(t *testing.T) {
	srv := newTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandlePromote_Success(t *testing.T) {
	srv := newTestServer(
		func(_ context.Context) error { return nil },
		nil,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/promote", nil)
	w := httptest.NewRecorder()
	srv.handlePromote(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "promoted", w.Body.String())
}

func TestHandlePromote_Error(t *testing.T) {
	srv := newTestServer(
		func(_ context.Context) error { return errors.New("redis error") },
		nil,
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/promote", nil)
	w := httptest.NewRecorder()
	srv.handlePromote(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "promote failed")
}

func TestHandlePromote_WrongMethod(t *testing.T) {
	srv := newTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/promote", nil)
	w := httptest.NewRecorder()
	srv.handlePromote(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandleDemote_Success(t *testing.T) {
	var capturedIP string
	var capturedPort int
	srv := newTestServer(
		nil,
		func(_ context.Context, ip string, port int) error {
			capturedIP = ip
			capturedPort = port
			return nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/demote?primaryIP=10.0.0.1&port=6379", nil)
	w := httptest.NewRecorder()
	srv.handleDemote(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "demoted", w.Body.String())
	assert.Equal(t, "10.0.0.1", capturedIP)
	assert.Equal(t, 6379, capturedPort)
}

func TestHandleDemote_DefaultPort(t *testing.T) {
	var capturedPort int
	srv := newTestServer(
		nil,
		func(_ context.Context, _ string, port int) error {
			capturedPort = port
			return nil
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/demote?primaryIP=10.0.0.1", nil)
	w := httptest.NewRecorder()
	srv.handleDemote(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 6379, capturedPort)
}

func TestHandleDemote_MissingPrimaryIP(t *testing.T) {
	srv := newTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/demote", nil)
	w := httptest.NewRecorder()
	srv.handleDemote(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "missing primaryIP")
}

func TestHandleDemote_InvalidPort(t *testing.T) {
	srv := newTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/demote?primaryIP=10.0.0.1&port=abc", nil)
	w := httptest.NewRecorder()
	srv.handleDemote(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid port")
}

func TestHandleDemote_WrongMethod(t *testing.T) {
	srv := newTestServer(nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/demote?primaryIP=10.0.0.1", nil)
	w := httptest.NewRecorder()
	srv.handleDemote(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandleDemote_Error(t *testing.T) {
	srv := newTestServer(
		nil,
		func(_ context.Context, _ string, _ int) error { return errors.New("replicaof failed") },
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/demote?primaryIP=10.0.0.1", nil)
	w := httptest.NewRecorder()
	srv.handleDemote(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "demote failed")
}

func TestWritePrometheusMetrics_Master(t *testing.T) {
	info := "# Server\r\n" +
		"redis_version:7.2.0\r\n" +
		"uptime_in_seconds:3600\r\n" +
		"# Clients\r\n" +
		"connected_clients:10\r\n" +
		"blocked_clients:2\r\n" +
		"# Memory\r\n" +
		"used_memory:1048576\r\n" +
		"used_memory_peak:2097152\r\n" +
		"# Replication\r\n" +
		"role:master\r\n" +
		"connected_slaves:2\r\n" +
		"master_repl_offset:12345\r\n" +
		"# Stats\r\n" +
		"keyspace_hits:100\r\n" +
		"keyspace_misses:5\r\n" +
		"total_commands_processed:200\r\n"

	w := httptest.NewRecorder()
	writePrometheusMetrics(w, info)
	output := w.Body.String()

	// Verify key metrics are present.
	assert.Contains(t, output, "redis_connected_clients 10")
	assert.Contains(t, output, "redis_blocked_clients 2")
	assert.Contains(t, output, "redis_used_memory_bytes 1048576")
	assert.Contains(t, output, "redis_used_memory_peak_bytes 2097152")
	assert.Contains(t, output, "redis_connected_replicas 2")
	assert.Contains(t, output, "redis_replication_offset 12345")
	assert.Contains(t, output, "redis_uptime_seconds 3600")
	assert.Contains(t, output, "redis_keyspace_hits_total 100")
	assert.Contains(t, output, "redis_keyspace_misses_total 5")
	assert.Contains(t, output, "redis_commands_processed_total 200")

	// Role: master -> 1
	assert.Contains(t, output, "redis_replication_role 1")

	// Verify HELP and TYPE lines exist.
	assert.Contains(t, output, "# HELP redis_connected_clients")
	assert.Contains(t, output, "# TYPE redis_connected_clients gauge")
}

func TestWritePrometheusMetrics_Slave(t *testing.T) {
	info := "# Replication\r\n" +
		"role:slave\r\n" +
		"connected_slaves:0\r\n"

	w := httptest.NewRecorder()
	writePrometheusMetrics(w, info)
	output := w.Body.String()

	// Role: slave -> 0
	assert.Contains(t, output, "redis_replication_role 0")
}

func TestWritePrometheusMetrics_Empty(t *testing.T) {
	w := httptest.NewRecorder()
	writePrometheusMetrics(w, "")
	output := w.Body.String()

	// Should not panic, output may be empty.
	assert.NotNil(t, output)
}

func TestWritePrometheusMetrics_PartialData(t *testing.T) {
	info := "# Memory\r\nused_memory:999\r\n"

	w := httptest.NewRecorder()
	writePrometheusMetrics(w, info)
	output := w.Body.String()

	assert.Contains(t, output, "redis_used_memory_bytes 999")
	// Other metrics should not be present.
	assert.True(t, !strings.Contains(output, "redis_connected_clients"))
}

// --- handleReadyz tests ---

func TestHandleReadyz_PingSuccess(t *testing.T) {
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	srv := &Server{
		redisClient: client,
		listenAddr:  ":0",
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	srv.handleReadyz(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok", w.Body.String())
}

func TestHandleReadyz_PingFails(t *testing.T) {
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })

	// Close miniredis to make PING fail.
	mr.Close()

	srv := &Server{
		redisClient: client,
		listenAddr:  ":0",
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	srv.handleReadyz(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "redis not ready")
}

// --- handleStatus JSON body tests (requires fakeRedisServer for INFO replication) ---

// fakeRedisForStatus is a minimal RESP server for testing handleStatus and handleMetrics.
type fakeRedisForStatus struct {
	listener net.Listener
	role     string
	offset   int64
}

func newFakeRedisForStatus(t *testing.T, role string, offset int64) (*fakeRedisForStatus, *goredis.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &fakeRedisForStatus{
		listener: ln,
		role:     role,
		offset:   offset,
	}
	go srv.serve()
	t.Cleanup(func() { ln.Close() })

	client := goredis.NewClient(&goredis.Options{
		Addr:            ln.Addr().String(),
		Protocol:        2,
		DisableIdentity: true,
	})
	t.Cleanup(func() { client.Close() })
	return srv, client
}

func (s *fakeRedisForStatus) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeRedisForStatus) handleConn(conn net.Conn) {
	defer conn.Close()
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
			conn.Write([]byte(resp))
		case "PING":
			conn.Write([]byte("+PONG\r\n"))
		case "CLIENT":
			conn.Write([]byte("+OK\r\n"))
		case "INFO":
			var info string
			if len(args) > 1 && strings.ToLower(args[1]) == "replication" {
				if s.role == "master" {
					info = fmt.Sprintf("# Replication\r\nrole:master\r\nconnected_slaves:2\r\nmaster_repl_offset:%d\r\n", s.offset)
				} else {
					info = fmt.Sprintf("# Replication\r\nrole:slave\r\nmaster_link_status:up\r\nslave_repl_offset:%d\r\n", s.offset)
				}
			} else {
				// INFO all -- for handleMetrics.
				info = "# Server\r\nredis_version:7.2.0\r\nuptime_in_seconds:3600\r\n" +
					"# Clients\r\nconnected_clients:5\r\nblocked_clients:1\r\n" +
					"# Memory\r\nused_memory:524288\r\nused_memory_peak:1048576\r\n" +
					"# Replication\r\nrole:master\r\nconnected_slaves:2\r\nmaster_repl_offset:9999\r\n" +
					"# Stats\r\nkeyspace_hits:50\r\nkeyspace_misses:10\r\ntotal_commands_processed:100\r\n"
			}
			conn.Write([]byte(fmt.Sprintf("$%d\r\n%s\r\n", len(info), info)))
		default:
			conn.Write([]byte("+OK\r\n"))
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return nil, nil
	}
	if line[0] == '*' {
		count, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("bad array count: %w", err)
		}
		args := make([]string, 0, count)
		for i := 0; i < count; i++ {
			arg, err := readBulkString(r)
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
		}
		return args, nil
	}
	return strings.Fields(line), nil
}

func readBulkString(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != '$' {
		return "", fmt.Errorf("expected bulk string, got: %q", line)
	}
	length, err := strconv.Atoi(line[1:])
	if err != nil {
		return "", fmt.Errorf("bad bulk string length: %w", err)
	}
	data := make([]byte, length+2)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", err
	}
	return string(data[:length]), nil
}

func TestHandleStatus_MasterJSON(t *testing.T) {
	_, client := newFakeRedisForStatus(t, "master", 12345)

	srv := &Server{
		redisClient: client,
		listenAddr:  ":0",
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var resp StatusResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "master", resp.Role)
	assert.Equal(t, int64(12345), resp.ReplicationOffset)
	assert.Equal(t, 2, resp.ConnectedReplicas)
	assert.True(t, resp.Connected)
	assert.Empty(t, resp.MasterLinkStatus)
}

func TestHandleStatus_SlaveJSON(t *testing.T) {
	_, client := newFakeRedisForStatus(t, "slave", 5000)

	srv := &Server{
		redisClient: client,
		listenAddr:  ":0",
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	srv.handleStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp StatusResponse
	err := json.NewDecoder(w.Body).Decode(&resp)
	require.NoError(t, err)

	assert.Equal(t, "slave", resp.Role)
	assert.Equal(t, int64(5000), resp.ReplicationOffset)
	assert.Equal(t, "up", resp.MasterLinkStatus)
	assert.True(t, resp.Connected)
}

func TestHandleMetrics_WithRedisInfo(t *testing.T) {
	_, client := newFakeRedisForStatus(t, "master", 9999)

	srv := &Server{
		redisClient: client,
		listenAddr:  ":0",
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")

	output := w.Body.String()
	assert.Contains(t, output, "redis_connected_clients 5")
	assert.Contains(t, output, "redis_used_memory_bytes 524288")
	assert.Contains(t, output, "redis_replication_role 1")
	assert.Contains(t, output, "redis_uptime_seconds 3600")
}

func TestHandleMetrics_RedisDown(t *testing.T) {
	mr := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { client.Close() })
	mr.Close()

	srv := &Server{
		redisClient: client,
		listenAddr:  ":0",
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.handleMetrics(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "failed to get INFO all")
}

func TestHandleHealthz_ProcessExited(t *testing.T) {
	srv := newTestServer(nil, nil)

	// Create a command that exits immediately.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait()) // Wait for it to finish so ProcessState is set.

	srv.SetRedisCmd(cmd)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	srv.handleHealthz(w, req)

	// ProcessState is non-nil after Wait(), so the process has exited.
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "redis-server not running")
}
