package replication

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRedisServer is a minimal RESP server for testing replication functions.
type fakeRedisServer struct {
	mu            sync.Mutex
	listener      net.Listener
	role          string
	replOffset    int64
	linkStatus    string
	slaveOfCalled bool
	slaveOfHost   string
	slaveOfPort   string
}

func newFakeRedisServer(t *testing.T, role string, offset int64, linkStatus string) (*fakeRedisServer, *redis.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &fakeRedisServer{
		listener:   ln,
		role:       role,
		replOffset: offset,
		linkStatus: linkStatus,
	}
	go srv.serve()
	t.Cleanup(func() { _ = ln.Close() })

	client := redis.NewClient(&redis.Options{
		Addr:            ln.Addr().String(),
		Protocol:        2,
		DisableIdentity: true,
	})
	t.Cleanup(func() { _ = client.Close() })

	return srv, client
}

func (s *fakeRedisServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *fakeRedisServer) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)

	for {
		args, err := readRESPCommand(reader)
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
			_, _ = conn.Write([]byte(resp))
		case "PING":
			_, _ = conn.Write([]byte("+PONG\r\n"))
		case "CLIENT":
			_, _ = conn.Write([]byte("+OK\r\n"))
		case "INFO":
			s.mu.Lock()
			role := s.role
			offset := s.replOffset
			link := s.linkStatus
			s.mu.Unlock()
			var info string
			if role == "master" {
				info = fmt.Sprintf("# Replication\r\nrole:master\r\nconnected_slaves:2\r\nmaster_repl_offset:%d\r\n", offset)
			} else {
				info = fmt.Sprintf("# Replication\r\nrole:slave\r\nmaster_link_status:%s\r\nslave_repl_offset:%d\r\nmaster_last_io_seconds_ago:0\r\n", link, offset)
			}
			_, _ = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(info), info)
		case "SLAVEOF", "REPLICAOF":
			if len(args) < 3 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments\r\n"))
				continue
			}
			s.mu.Lock()
			s.slaveOfCalled = true
			s.slaveOfHost = args[1]
			s.slaveOfPort = args[2]
			if strings.ToUpper(args[1]) == "NO" && strings.ToUpper(args[2]) == "ONE" {
				s.role = "master"
			} else {
				s.role = "slave"
			}
			s.mu.Unlock()
			_, _ = conn.Write([]byte("+OK\r\n"))
		default:
			_, _ = conn.Write([]byte("+OK\r\n"))
		}
	}
}

func readRESPCommand(r *bufio.Reader) ([]string, error) {
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
			arg, err := readRESPBulkString(r)
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
		}
		return args, nil
	}
	return []string{line}, nil
}

func readRESPBulkString(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != '$' {
		return "", fmt.Errorf("expected bulk string, got: %s", line)
	}
	length, err := strconv.Atoi(line[1:])
	if err != nil {
		return "", fmt.Errorf("bad bulk string length: %w", err)
	}
	buf := make([]byte, length+2) // +2 for \r\n
	_, err = r.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:length]), nil
}

// --- parseInfo tests (pre-existing) ---

func TestParseInfo_Master(t *testing.T) {
	raw := "# Replication\r\n" +
		"role:master\r\n" +
		"connected_slaves:2\r\n" +
		"master_repl_offset:12345\r\n" +
		"slave0:ip=10.0.0.2,port=6379,state=online,offset=12340,lag=0\r\n" +
		"slave1:ip=10.0.0.3,port=6379,state=online,offset=12340,lag=1\r\n"

	info := parseInfo(raw)

	assert.Equal(t, "master", info.Role)
	assert.Equal(t, 2, info.ConnectedReplicas)
	assert.Equal(t, int64(12345), info.MasterReplOffset)
	assert.Equal(t, int64(0), info.SlaveReplOffset)
	assert.Empty(t, info.MasterLinkStatus)
}

func TestParseInfo_Slave(t *testing.T) {
	raw := "# Replication\r\n" +
		"role:slave\r\n" +
		"master_host:10.0.0.1\r\n" +
		"master_port:6379\r\n" +
		"master_link_status:up\r\n" +
		"slave_repl_offset:12340\r\n" +
		"master_last_io_seconds_ago:0\r\n"

	info := parseInfo(raw)

	assert.Equal(t, "slave", info.Role)
	assert.Equal(t, "up", info.MasterLinkStatus)
	assert.Equal(t, int64(12340), info.SlaveReplOffset)
	assert.Equal(t, 0, info.MasterLastIOSecondsAgo)
}

func TestParseInfo_Empty(t *testing.T) {
	info := parseInfo("")
	assert.Equal(t, "", info.Role)
	assert.Equal(t, 0, info.ConnectedReplicas)
	assert.Equal(t, int64(0), info.MasterReplOffset)
}

func TestParseInfo_CommentsAndBlankLines(t *testing.T) {
	raw := "# Server\r\n" +
		"\r\n" +
		"# Replication\r\n" +
		"role:master\r\n" +
		"\r\n" +
		"connected_slaves:1\r\n" +
		"# Stats\r\n"

	info := parseInfo(raw)

	assert.Equal(t, "master", info.Role)
	assert.Equal(t, 1, info.ConnectedReplicas)
}

func TestParseInfo_Malformed(t *testing.T) {
	raw := "this is not valid redis info\r\nno-colons-here\r\n"
	info := parseInfo(raw)
	assert.Equal(t, "", info.Role)
}

func TestParseInfo_SlaveWithDisconnectedLink(t *testing.T) {
	raw := "# Replication\r\n" +
		"role:slave\r\n" +
		"master_link_status:down\r\n" +
		"slave_repl_offset:5000\r\n" +
		"master_last_io_seconds_ago:120\r\n"

	info := parseInfo(raw)

	assert.Equal(t, "slave", info.Role)
	assert.Equal(t, "down", info.MasterLinkStatus)
	assert.Equal(t, int64(5000), info.SlaveReplOffset)
	assert.Equal(t, 120, info.MasterLastIOSecondsAgo)
}

func TestParseInfo_MasterWithZeroReplicas(t *testing.T) {
	raw := "# Replication\r\n" +
		"role:master\r\n" +
		"connected_slaves:0\r\n" +
		"master_repl_offset:0\r\n"

	info := parseInfo(raw)

	assert.Equal(t, "master", info.Role)
	assert.Equal(t, 0, info.ConnectedReplicas)
	assert.Equal(t, int64(0), info.MasterReplOffset)
}

// --- Integration tests using fake RESP server ---

func TestGetInfo_Master(t *testing.T) {
	_, client := newFakeRedisServer(t, "master", 12345, "")
	ctx := context.Background()

	info, err := GetInfo(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, "master", info.Role)
	assert.Equal(t, 2, info.ConnectedReplicas)
	assert.Equal(t, int64(12345), info.MasterReplOffset)
}

func TestGetInfo_Slave(t *testing.T) {
	_, client := newFakeRedisServer(t, "slave", 5000, "up")
	ctx := context.Background()

	info, err := GetInfo(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, "slave", info.Role)
	assert.Equal(t, "up", info.MasterLinkStatus)
	assert.Equal(t, int64(5000), info.SlaveReplOffset)
}

func TestGetInfo_ErrorOnClosedClient(t *testing.T) {
	_, client := newFakeRedisServer(t, "master", 0, "")
	_ = client.Close()
	ctx := context.Background()

	_, err := GetInfo(ctx, client)
	require.Error(t, err)
}

func TestSetReplicaOf(t *testing.T) {
	srv, client := newFakeRedisServer(t, "master", 0, "")
	ctx := context.Background()

	err := SetReplicaOf(ctx, client, "10.0.0.1", 6379)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.True(t, srv.slaveOfCalled)
	assert.Equal(t, "10.0.0.1", srv.slaveOfHost)
	assert.Equal(t, "6379", srv.slaveOfPort)
	assert.Equal(t, "slave", srv.role)
}

func TestPromote(t *testing.T) {
	srv, client := newFakeRedisServer(t, "slave", 5000, "up")
	ctx := context.Background()

	err := Promote(ctx, client)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.True(t, srv.slaveOfCalled)
	assert.Equal(t, "NO", srv.slaveOfHost)
	assert.Equal(t, "ONE", srv.slaveOfPort)
	assert.Equal(t, "master", srv.role)
}

func TestReplicationOffset_Master(t *testing.T) {
	_, client := newFakeRedisServer(t, "master", 12345, "")
	ctx := context.Background()

	offset, err := ReplicationOffset(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, int64(12345), offset)
}

func TestReplicationOffset_Slave(t *testing.T) {
	_, client := newFakeRedisServer(t, "slave", 9876, "up")
	ctx := context.Background()

	offset, err := ReplicationOffset(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, int64(9876), offset)
}

func TestReplicationOffset_ErrorOnClosedClient(t *testing.T) {
	_, client := newFakeRedisServer(t, "master", 0, "")
	_ = client.Close()
	ctx := context.Background()

	_, err := ReplicationOffset(ctx, client)
	require.Error(t, err)
}

func TestIsConnectedToPrimary_Up(t *testing.T) {
	_, client := newFakeRedisServer(t, "slave", 5000, "up")
	ctx := context.Background()

	connected, err := IsConnectedToPrimary(ctx, client)
	require.NoError(t, err)
	assert.True(t, connected)
}

func TestIsConnectedToPrimary_Down(t *testing.T) {
	_, client := newFakeRedisServer(t, "slave", 5000, "down")
	ctx := context.Background()

	connected, err := IsConnectedToPrimary(ctx, client)
	require.NoError(t, err)
	assert.False(t, connected)
}

func TestIsConnectedToPrimary_Master(t *testing.T) {
	_, client := newFakeRedisServer(t, "master", 0, "")
	ctx := context.Background()

	connected, err := IsConnectedToPrimary(ctx, client)
	require.NoError(t, err)
	assert.False(t, connected, "master has no master_link_status")
}

func TestIsConnectedToPrimary_ErrorOnClosedClient(t *testing.T) {
	_, client := newFakeRedisServer(t, "master", 0, "")
	_ = client.Close()
	ctx := context.Background()

	_, err := IsConnectedToPrimary(ctx, client)
	require.Error(t, err)
}
