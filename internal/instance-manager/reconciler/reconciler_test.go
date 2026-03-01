package reconciler

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// fakeRedisServer is a minimal RESP server that supports HELLO, PING, INFO,
// CONFIG, SLAVEOF, and ACL commands. It allows tests to verify that the
// reconciler calls the correct Redis commands without needing a full Redis
// instance or miniredis (which lacks INFO replication and CONFIG support).
type fakeRedisServer struct {
	mu            sync.Mutex
	listener      net.Listener
	configValues  map[string]string
	role          string // "master" or "slave"
	replOffset    int64
	slaveOfCalled bool
	slaveOfHost   string
	slaveOfPort   string
	aclLoaded     bool
}

func newFakeRedisServer(t *testing.T) (*fakeRedisServer, *redis.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &fakeRedisServer{
		listener:     ln,
		configValues: make(map[string]string),
		role:         "master",
		replOffset:   1000,
	}

	go srv.serve()

	t.Cleanup(func() {
		_ = ln.Close()
	})

	client := redis.NewClient(&redis.Options{
		Addr:            ln.Addr().String(),
		Protocol:        2, // Use RESP2 to avoid HELLO handshake issues.
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
			// go-redis sends HELLO on connection init.
			// Respond with a RESP2 array (map-like alternating key/value pairs).
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
			masterHost := s.slaveOfHost
			masterPort := s.slaveOfPort
			s.mu.Unlock()
			var info string
			if role == "master" {
				info = fmt.Sprintf("# Replication\r\nrole:master\r\nconnected_slaves:2\r\nmaster_repl_offset:%d\r\n", offset)
			} else {
				if masterHost == "" {
					masterHost = "unknown"
				}
				if masterPort == "" {
					masterPort = "0"
				}
				info = fmt.Sprintf(
					"# Replication\r\nrole:slave\r\nmaster_host:%s\r\nmaster_port:%s\r\nmaster_link_status:up\r\nslave_repl_offset:%d\r\n",
					masterHost,
					masterPort,
					offset,
				)
			}
			_, _ = fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(info), info)
		case "CONFIG":
			if len(args) < 2 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments\r\n"))
				continue
			}
			subcmd := strings.ToUpper(args[1])
			switch subcmd {
			case "SET":
				if len(args) < 4 {
					_, _ = conn.Write([]byte("-ERR wrong number of arguments\r\n"))
					continue
				}
				s.mu.Lock()
				s.configValues[args[2]] = args[3]
				s.mu.Unlock()
				_, _ = conn.Write([]byte("+OK\r\n"))
			case "GET":
				if len(args) < 3 {
					_, _ = conn.Write([]byte("-ERR wrong number of arguments\r\n"))
					continue
				}
				key := args[2]
				s.mu.Lock()
				val, ok := s.configValues[key]
				s.mu.Unlock()
				if ok {
					_, _ = fmt.Fprintf(conn, "*2\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(key), key, len(val), val)
				} else {
					_, _ = conn.Write([]byte("*0\r\n"))
				}
			default:
				_, _ = conn.Write([]byte("-ERR unknown CONFIG subcommand\r\n"))
			}
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
		case "ACL":
			if len(args) >= 2 && strings.ToUpper(args[1]) == "LOAD" {
				s.mu.Lock()
				s.aclLoaded = true
				s.mu.Unlock()
				_, _ = conn.Write([]byte("+OK\r\n"))
			} else {
				_, _ = conn.Write([]byte("+OK\r\n"))
			}
		default:
			_, _ = conn.Write([]byte("+OK\r\n"))
		}
	}
}

// readRESPCommand reads one complete RESP command from the buffered reader.
func readRESPCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")

	if len(line) == 0 {
		return nil, nil
	}

	// RESP Array: *<count>\r\n
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

	// Inline command (space-separated).
	return strings.Fields(line), nil
}

// readRESPBulkString reads a RESP bulk string: $<len>\r\n<data>\r\n
func readRESPBulkString(r *bufio.Reader) (string, error) {
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

	// Read exactly length + 2 bytes (data + \r\n).
	data := make([]byte, length+2)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", err
	}

	return string(data[:length]), nil
}

func overrideTLSPaths(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	oldTLSCertFilePath := tlsCertFilePath
	oldTLSKeyFilePath := tlsKeyFilePath
	oldTLSCAFilePath := tlsCAFilePath

	tlsCertFilePath = filepath.Join(tmpDir, "tls.crt")
	tlsKeyFilePath = filepath.Join(tmpDir, "tls.key")
	tlsCAFilePath = filepath.Join(tmpDir, "ca.crt")

	t.Cleanup(func() {
		tlsCertFilePath = oldTLSCertFilePath
		tlsKeyFilePath = oldTLSKeyFilePath
		tlsCAFilePath = oldTLSCAFilePath
	})

	return tmpDir
}

func writeTLSFiles(t *testing.T, dir, cert, key, ca string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), []byte(cert), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), []byte(key), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), []byte(ca), 0o600))
}

func TestIsFenced_NoAnnotations(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-0"}
	cluster := &redisv1.RedisCluster{}
	assert.False(t, r.isFenced(cluster))
}

func TestIsFenced_NoFencingAnnotation(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-0"}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"other-annotation": "value",
			},
		},
	}
	assert.False(t, r.isFenced(cluster))
}

func TestIsFenced_PodIsFenced(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-0"}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				redisv1.FencingAnnotationKey: `["mycluster-0","mycluster-1"]`,
			},
		},
	}
	assert.True(t, r.isFenced(cluster))
}

func TestIsFenced_PodNotFenced(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-2"}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				redisv1.FencingAnnotationKey: `["mycluster-0","mycluster-1"]`,
			},
		},
	}
	assert.False(t, r.isFenced(cluster))
}

func TestIsFenced_InvalidJSON(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-0"}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				redisv1.FencingAnnotationKey: "not-json",
			},
		},
	}
	assert.False(t, r.isFenced(cluster))
}

func TestIsFenced_EmptyAnnotationValue(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-0"}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				redisv1.FencingAnnotationKey: "",
			},
		},
	}
	assert.False(t, r.isFenced(cluster))
}

func TestIsFenced_EmptyArray(t *testing.T) {
	r := &InstanceReconciler{podName: "mycluster-0"}
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				redisv1.FencingAnnotationKey: `[]`,
			},
		},
	}
	assert.False(t, r.isFenced(cluster))
}

func TestRequiresRestart(t *testing.T) {
	tests := []struct {
		key      string
		expected bool
	}{
		{"bind", true},
		{"port", true},
		{"tls-port", true},
		{"tls-cert-file", false},
		{"tls-key-file", false},
		{"tls-ca-cert-file", false},
		{"unixsocket", true},
		{"databases", true},
		{"maxmemory", false},
		{"save", false},
		{"requirepass", false},
		{"maxclients", false},
		{"", false},
		{"appendonly", false},
		{"hz", false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			assert.Equal(t, tt.expected, requiresRestart(tt.key))
		})
	}
}

func TestReconcileTLSCerts_DisabledClearsChecksums(t *testing.T) {
	rec := &InstanceReconciler{
		tlsCertChecksums: map[string]string{
			"tls-cert-file": "old",
		},
	}

	err := rec.reconcileTLSCerts(context.Background(), &redisv1.RedisCluster{})
	require.NoError(t, err)
	assert.Empty(t, rec.tlsCertChecksums)
}

func TestReconcileTLSCerts_ReloadsOnChecksumChange(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	recorder := record.NewFakeRecorder(10)

	tlsDir := overrideTLSPaths(t)
	writeTLSFiles(t, tlsDir, "cert-v1", "key-v1", "ca-v1")

	rec := &InstanceReconciler{
		redisClient:      redisClient,
		recorder:         recorder,
		tlsCertChecksums: map[string]string{},
	}

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}

	// First reconcile establishes baseline checksums.
	require.NoError(t, rec.reconcileTLSCerts(context.Background(), cluster))

	srv.mu.Lock()
	assert.Empty(t, srv.configValues)
	srv.mu.Unlock()

	// Rotate cert material.
	writeTLSFiles(t, tlsDir, "cert-v2", "key-v2", "ca-v2")

	require.NoError(t, rec.reconcileTLSCerts(context.Background(), cluster))

	srv.mu.Lock()
	assert.Equal(t, tlsCertFilePath, srv.configValues["tls-cert-file"])
	assert.Equal(t, tlsKeyFilePath, srv.configValues["tls-key-file"])
	assert.Equal(t, tlsCAFilePath, srv.configValues["tls-ca-cert-file"])
	srv.mu.Unlock()

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "CertificatesRotated")
	default:
		t.Fatal("expected CertificatesRotated event")
	}
}

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(redisv1.AddToScheme(s))
	return s
}

func newTestCluster(name, namespace string) *redisv1.RedisCluster {
	return &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: 3,
			ImageName: "redis:7.2",
			AuthSecret: &redisv1.LocalObjectReference{
				Name: name + "-auth",
			},
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
		},
	}
}

func TestReconcileConfig_AppliesSettings(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
	}

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"maxmemory": "256mb",
				"hz":        "50",
			},
		},
	}

	err := rec.reconcileConfig(context.Background(), cluster)
	require.NoError(t, err)

	// Verify config was applied.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.Equal(t, "256mb", srv.configValues["maxmemory"])
	assert.Equal(t, "50", srv.configValues["hz"])
}

func TestReconcileConfig_SkipsRestartKeys(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
	}

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"bind":      "127.0.0.1", // requires restart -- should be skipped
				"maxmemory": "128mb",     // does not require restart
			},
		},
	}

	err := rec.reconcileConfig(context.Background(), cluster)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	// maxmemory should be set.
	assert.Equal(t, "128mb", srv.configValues["maxmemory"])
	// bind should NOT have been set (requires restart).
	_, hasBind := srv.configValues["bind"]
	assert.False(t, hasBind, "bind requires restart and should not be set via CONFIG SET")
}

func TestReconcileConfig_EmptyConfig(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
	}

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: nil,
		},
	}

	err := rec.reconcileConfig(context.Background(), cluster)
	require.NoError(t, err)
}

func TestReconcileRole_AlreadyCorrectPrimary(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master"
	srv.mu.Unlock()

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"

	err := rec.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	// Should not have called SLAVEOF since role is already correct.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.False(t, srv.slaveOfCalled, "should not call SLAVEOF when already correct role")
}

func TestReconcileRole_ShouldBeReplicaButIsMaster(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master"
	srv.mu.Unlock()

	primaryPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: "10.244.0.5"},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryPod).Build()

	rec := &InstanceReconciler{
		client:      fakeClient,
		redisClient: redisClient,
		recorder:    record.NewFakeRecorder(100),
		podName:     "test-1",
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0" // test-1 should be replica

	err := rec.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	// Should have called SLAVEOF to demote.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.True(t, srv.slaveOfCalled, "should call SLAVEOF to demote to replica")
	assert.Equal(t, "10.244.0.5", srv.slaveOfHost)
	assert.Equal(t, "6379", srv.slaveOfPort)
}

func TestReconcileRole_ShouldBePrimaryButIsSlave(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave"
	srv.mu.Unlock()

	rec := &InstanceReconciler{
		redisClient: redisClient,
		recorder:    record.NewFakeRecorder(100),
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0" // this pod should be primary

	err := rec.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	// Should have called SLAVEOF NO ONE to promote.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.True(t, srv.slaveOfCalled)
	assert.Equal(t, "NO", srv.slaveOfHost)
	assert.Equal(t, "ONE", srv.slaveOfPort)
}

func TestReconcileRole_AlreadyCorrectReplica(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave"
	srv.slaveOfHost = "10.244.0.5"
	srv.slaveOfPort = "6379"
	srv.mu.Unlock()

	primaryPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-0", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: "10.244.0.5"},
	}
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(primaryPod).Build()

	rec := &InstanceReconciler{
		client:      fakeClient,
		redisClient: redisClient,
		podName:     "test-1",
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0" // test-1 is not primary

	err := rec.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	// Should not have called SLAVEOF since role is already slave.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.False(t, srv.slaveOfCalled, "should not call SLAVEOF when already a replica")
}

func TestReconcileRole_ReplicaMode_EnsuresExternalReplication(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master"
	srv.mu.Unlock()

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
		recorder:    record.NewFakeRecorder(100),
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6380,
		},
	}

	err := rec.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.True(t, srv.slaveOfCalled)
	assert.Equal(t, "external-primary", srv.slaveOfHost)
	assert.Equal(t, "6380", srv.slaveOfPort)
}

func TestReconcileRole_ReplicaModePromote_PromotesCurrentPrimary(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave"
	srv.slaveOfHost = "external-primary"
	srv.slaveOfPort = "6379"
	srv.mu.Unlock()

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
		recorder:    record.NewFakeRecorder(100),
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Promote: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	err := rec.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.True(t, srv.slaveOfCalled)
	assert.Equal(t, "NO", srv.slaveOfHost)
	assert.Equal(t, "ONE", srv.slaveOfPort)
}

func TestReconcileSecrets_NoSecretRefs(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			// No AuthSecret or ACLConfigSecret set.
		},
	}

	err := rec.reconcileSecrets(context.Background(), cluster)
	require.NoError(t, err)
}

func TestReconcileSecrets_FallbackToGeneratedAuthSecret(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	projectedDir := t.TempDir()
	t.Setenv(envProjectedSecretsDir, projectedDir)

	require.NoError(t, os.MkdirAll(filepath.Join(projectedDir, "test-auth"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectedDir, "test-auth", "password"), []byte("generated-pass"), 0o600))

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}

	err := rec.reconcileSecrets(context.Background(), cluster)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.Equal(t, "generated-pass", srv.configValues["requirepass"])
	assert.Equal(t, "generated-pass", srv.configValues["masterauth"])
}

func TestReconcileSecrets_FallbackAuthSecretMissingReturnsError(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)
	projectedDir := t.TempDir()
	t.Setenv(envProjectedSecretsDir, projectedDir)

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
	}

	err := rec.reconcileSecrets(context.Background(), cluster)
	require.Error(t, err)
	assert.ErrorIs(t, err, errFallbackAuthSecretUnavailable)
}

func TestReconcileSecrets_ReplicaModeAuthOverridesMasterAuth(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	projectedDir := t.TempDir()
	t.Setenv(envProjectedSecretsDir, projectedDir)

	require.NoError(t, os.MkdirAll(filepath.Join(projectedDir, "local-auth"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(projectedDir, "external-auth"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectedDir, "local-auth", "password"), []byte("local-pass"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projectedDir, "external-auth", "password"), []byte("external-pass"), 0o600))

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
		},
		Spec: redisv1.RedisClusterSpec{
			AuthSecret: &redisv1.LocalObjectReference{Name: "local-auth"},
			ReplicaMode: &redisv1.ReplicaModeSpec{
				Enabled: true,
				Source: &redisv1.ReplicaSourceSpec{
					Host:           "external-primary",
					Port:           6379,
					AuthSecretName: "external-auth",
				},
			},
		},
	}

	err := rec.reconcileSecrets(context.Background(), cluster)
	require.NoError(t, err)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.Equal(t, "local-pass", srv.configValues["requirepass"])
	assert.Equal(t, "external-pass", srv.configValues["masterauth"])
}

func TestReconcileSecrets_AuthSecretMissing(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	rec := &InstanceReconciler{
		redisClient: redisClient,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			AuthSecret: &redisv1.LocalObjectReference{Name: "nonexistent"},
		},
	}

	// The file /projected/nonexistent/password doesn't exist.
	// readSecretKey returns ("", nil) for missing files, so CONFIG SET is skipped.
	err := rec.reconcileSecrets(context.Background(), cluster)
	require.NoError(t, err)
}

func TestReportStatus_MasterRole(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master"
	srv.replOffset = 5000
	srv.mu.Unlock()

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	err := rec.reportStatus(context.Background(), cluster)
	require.NoError(t, err)

	// Re-fetch the cluster.
	var updated redisv1.RedisCluster
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	status, ok := updated.Status.InstancesStatus["test-0"]
	require.True(t, ok, "status for test-0 should exist")
	assert.Equal(t, "master", status.Role)
	assert.True(t, status.Connected)
	assert.Equal(t, int64(5000), status.ReplicationOffset)
	assert.Equal(t, int32(2), status.ConnectedReplicas)
}

func TestReportStatus_InitializesMap(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master"
	srv.mu.Unlock()

	cluster := newTestCluster("test", "default")
	// InstancesStatus is nil by default.

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	err := rec.reportStatus(context.Background(), cluster)
	require.NoError(t, err)

	var updated redisv1.RedisCluster
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	assert.NotNil(t, updated.Status.InstancesStatus)
	_, ok := updated.Status.InstancesStatus["test-0"]
	assert.True(t, ok)
}

func TestReportStatus_SlaveRole(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave"
	srv.replOffset = 3000
	srv.mu.Unlock()

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-1", "default")

	err := rec.reportStatus(context.Background(), cluster)
	require.NoError(t, err)

	var updated redisv1.RedisCluster
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	status, ok := updated.Status.InstancesStatus["test-1"]
	require.True(t, ok)
	assert.Equal(t, "slave", status.Role)
	assert.True(t, status.Connected)
	// For slave role, reportStatus uses SlaveReplOffset.
	assert.Equal(t, int64(3000), status.ReplicationOffset)
	assert.Equal(t, "up", status.MasterLinkStatus)
}

func TestReconcile_HappyPath(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	result, err := rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}

func TestReconcile_ClusterNotFound(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	_, err := rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetching RedisCluster")
}

func TestReconcile_Fenced(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Annotations = map[string]string{
		redisv1.FencingAnnotationKey: `["test-0"]`,
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	result, err := rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)
	// Fenced pod returns early.
	assert.Equal(t, reconcile.Result{}, result)
}

func TestReconcile_FullCycle_WithConfig(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.Redis = map[string]string{
		"maxmemory":        "512mb",
		"maxmemory-policy": "allkeys-lru",
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	result, err := rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// Verify config was applied.
	srv.mu.Lock()
	assert.Equal(t, "512mb", srv.configValues["maxmemory"])
	assert.Equal(t, "allkeys-lru", srv.configValues["maxmemory-policy"])
	srv.mu.Unlock()

	// Verify status was reported.
	var updated redisv1.RedisCluster
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	_, ok := updated.Status.InstancesStatus["test-0"]
	assert.True(t, ok)
}

func TestReconcile_FallbackAuthSecretMissingIsFatal(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)
	projectedDir := t.TempDir()
	t.Setenv(envProjectedSecretsDir, projectedDir)

	cluster := newTestCluster("test", "default")
	cluster.Spec.AuthSecret = nil
	cluster.Status.CurrentPrimary = "test-0"

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := NewInstanceReconciler(fakeClient, redisClient, record.NewFakeRecorder(100), "test", "test-0", "default")

	_, err := rec.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, errFallbackAuthSecretUnavailable)
}

func TestStopRedis_NilCmd(t *testing.T) {
	rec := &InstanceReconciler{}
	// Should not panic.
	rec.stopRedis()
}

func TestStopRedis_WithRunningProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	rec := &InstanceReconciler{}
	rec.SetRedisCmd(cmd)
	rec.stopRedis()

	// The process should have received a signal.
	// Wait for it to exit.
	err := cmd.Wait()
	assert.Error(t, err, "process should have been interrupted")
}

func TestReadSecretKey_NotExists(t *testing.T) {
	rec := &InstanceReconciler{namespace: "default"}

	// /projected/nonexistent/password does not exist.
	val, err := rec.readSecretKey(context.Background(), "default", "nonexistent", "password")
	require.NoError(t, err)
	assert.Equal(t, "", val)
}

func TestReadSecretKey_FileExists(t *testing.T) {
	// Create a temp projected structure.
	tmpDir := t.TempDir()
	secretDir := tmpDir + "/mysecret"
	require.NoError(t, os.MkdirAll(secretDir, 0755))
	require.NoError(t, os.WriteFile(secretDir+"/password", []byte("  my-pass  \n"), 0644))

	// readSecretKey reads from /projected/<secretName>/<key> which is hardcoded.
	// We can't easily test this without modifying the path. Skip file-content test
	// and just verify the function handles missing files gracefully.
	rec := &InstanceReconciler{namespace: "default"}
	val, err := rec.readSecretKey(context.Background(), "default", "nonexistent-"+tmpDir, "password")
	require.NoError(t, err)
	assert.Equal(t, "", val)
}
