//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
)

func TestTLSReplicationAndCertRotation(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	caPEM, caKey, caCert := generateTestCA(t)

	primaryCertPEM, primaryKeyPEM := generateServerCert(t, caKey, caCert, []string{"primary", "localhost"})
	replicaCertPEM, replicaKeyPEM := generateServerCert(t, caKey, caCert, []string{"replica", "localhost"})

	primaryTLSDir := writeTLSFiles(t, "primary-tls-", primaryCertPEM, primaryKeyPEM, caPEM)
	replicaTLSDir := writeTLSFiles(t, "replica-tls-", replicaCertPEM, replicaKeyPEM, caPEM)

	network, err := tcnetwork.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = network.Remove(context.Background())
	})

	redisTLSArgs := []string{
		"redis-server",
		"--port", "0",
		"--tls-port", "6379",
		"--tls-cert-file", "/tls/tls.crt",
		"--tls-key-file", "/tls/tls.key",
		"--tls-ca-cert-file", "/tls/ca.crt",
		"--tls-auth-clients", "no",
		"--tls-replication", "yes",
	}

	primaryContainer := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"primary"}, network),
		testcontainers.WithMounts(testcontainers.BindMount(primaryTLSDir, testcontainers.ContainerMountTarget("/tls"))),
		testcontainers.WithCmd(redisTLSArgs...),
	)
	replicaContainer := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"replica"}, network),
		testcontainers.WithMounts(testcontainers.BindMount(replicaTLSDir, testcontainers.ContainerMountTarget("/tls"))),
		testcontainers.WithCmd(redisTLSArgs...),
	)

	primaryTLSClient := newRedisTLSClient(ctx, t, primaryContainer, caPEM)
	replicaTLSClient := newRedisTLSClient(ctx, t, replicaContainer, caPEM)
	waitForRedisReady(t, ctx, primaryTLSClient)
	waitForRedisReady(t, ctx, replicaTLSClient)

	plainPrimaryClient := newRedisClient(ctx, t, primaryContainer, "", "")
	require.Error(t, plainPrimaryClient.Ping(ctx).Err(), "plain-text client must be rejected when TLS is enabled")

	exitCode, output, err := primaryContainer.Exec(ctx, []string{
		"redis-cli",
		"--tls",
		"--cacert", "/tls/ca.crt",
		"-h", "127.0.0.1",
		"-p", "6379",
		"PING",
	})
	require.NoError(t, err)
	require.Equal(t, 0, exitCode)
	outputBytes, err := io.ReadAll(output)
	require.NoError(t, err)
	require.Contains(t, strings.ToUpper(string(outputBytes)), "PONG")

	require.NoError(t, replicaTLSClient.SlaveOf(ctx, "primary", "6379").Err())

	require.Eventually(t, func() bool {
		info, infoErr := replicaTLSClient.Info(ctx, "replication").Result()
		return infoErr == nil && strings.Contains(info, "role:slave") && strings.Contains(info, "master_link_status:up")
	}, 30*time.Second, 250*time.Millisecond)

	require.NoError(t, primaryTLSClient.Set(ctx, "tls-repl", "value-v1", 0).Err())
	require.Eventually(t, func() bool {
		value, getErr := replicaTLSClient.Get(ctx, "tls-repl").Result()
		return getErr == nil && value == "value-v1"
	}, 30*time.Second, 250*time.Millisecond)

	processIDBefore := redisProcessID(t, ctx, primaryTLSClient)
	serialBefore := serverSerialNumber(t, ctx, primaryContainer, caPEM)

	rotatedCertPEM, rotatedKeyPEM := generateServerCert(t, caKey, caCert, []string{"primary", "localhost"})
	// Write rotated certs to new paths; Redis CONFIG SET may not reload when path is unchanged.
	rotatedCertPath := filepath.Join(primaryTLSDir, "tls-rotated.crt")
	rotatedKeyPath := filepath.Join(primaryTLSDir, "tls-rotated.key")
	require.NoError(t, os.WriteFile(rotatedCertPath, rotatedCertPEM, 0o644))
	require.NoError(t, os.WriteFile(rotatedKeyPath, rotatedKeyPEM, 0o644))

	// Set cert and key in one CONFIG SET so Redis applies both atomically; loading one without
	// the matching pair causes "Unable to update TLS configuration".
	require.NoError(t, primaryTLSClient.Do(ctx, "CONFIG", "SET",
		"tls-cert-file", "/tls/tls-rotated.crt",
		"tls-key-file", "/tls/tls-rotated.key",
	).Err())

	require.Equal(t, processIDBefore, redisProcessID(t, ctx, primaryTLSClient), "cert reload should not restart redis process")

	require.Eventually(t, func() bool {
		return serverSerialNumber(t, ctx, primaryContainer, caPEM).Cmp(serialBefore) != 0
	}, 20*time.Second, 250*time.Millisecond)

	require.NoError(t, primaryTLSClient.Set(ctx, "tls-repl", "value-v2", 0).Err())
	require.Eventually(t, func() bool {
		value, getErr := replicaTLSClient.Get(ctx, "tls-repl").Result()
		return getErr == nil && value == "value-v2"
	}, 30*time.Second, 250*time.Millisecond)
}

func writeTLSFiles(t *testing.T, prefix string, certPEM, keyPEM, caPEM []byte) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o755))
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})

	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls.key"), keyPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.crt"), caPEM, 0o644))

	return dir
}

func newRedisTLSClient(
	ctx context.Context,
	t *testing.T,
	container testcontainers.Container,
	caPEM []byte,
) *redis.Client {
	t.Helper()

	host, err := container.Host(ctx)
	require.NoError(t, err)

	mappedPort, err := container.MappedPort(ctx, redisPort)
	require.NoError(t, err)

	rootCAs := x509.NewCertPool()
	require.True(t, rootCAs.AppendCertsFromPEM(caPEM))

	client := redis.NewClient(&redis.Options{
		Addr: net.JoinHostPort(host, mappedPort.Port()),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
			ServerName: "localhost",
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func redisProcessID(t *testing.T, ctx context.Context, client *redis.Client) string {
	t.Helper()
	info, err := client.Info(ctx, "server").Result()
	require.NoError(t, err)

	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "process_id:") {
			return strings.TrimPrefix(line, "process_id:")
		}
	}
	t.Fatalf("process_id not found in INFO output: %s", info)
	return ""
}

func serverSerialNumber(
	t *testing.T,
	ctx context.Context,
	container testcontainers.Container,
	caPEM []byte,
) *big.Int {
	t.Helper()

	host, err := container.Host(ctx)
	require.NoError(t, err)

	mappedPort, err := container.MappedPort(ctx, redisPort)
	require.NoError(t, err)

	rootCAs := x509.NewCertPool()
	require.True(t, rootCAs.AppendCertsFromPEM(caPEM))

	conn, err := tls.Dial("tcp", net.JoinHostPort(host, mappedPort.Port()), &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		ServerName: "localhost",
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	require.NotEmpty(t, state.PeerCertificates)
	return state.PeerCertificates[0].SerialNumber
}

func generateTestCA(t *testing.T) ([]byte, *ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "redis-operator-integration-ca",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	caCert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return caPEM, caKey, caCert
}

func generateServerCert(t *testing.T, caKey *ecdsa.PrivateKey, caCert *x509.Certificate, dnsNames []string) ([]byte, []byte) {
	t.Helper()

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: dnsNames[0],
		},
		DNSNames:    dnsNames,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(2 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
