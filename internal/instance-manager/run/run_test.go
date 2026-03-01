package run

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// overrideDataDir sets dataDir and redisConfPath to a temp directory and restores them when the test ends.
func overrideDataDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	oldDataDir := dataDir
	oldConfPath := redisConfPath
	oldSentinelConfPath := sentinelConfPath
	dataDir = tmpDir
	redisConfPath = filepath.Join(tmpDir, "redis.conf")
	sentinelConfPath = filepath.Join(tmpDir, "sentinel.conf")
	t.Cleanup(func() {
		dataDir = oldDataDir
		redisConfPath = oldConfPath
		sentinelConfPath = oldSentinelConfPath
	})
	return tmpDir
}

func overrideTLSPaths(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	oldTLSCertPath := tlsCertPath
	oldTLSKeyPath := tlsKeyPath
	oldTLSCAPath := tlsCAPath

	tlsCertPath = filepath.Join(tmpDir, "tls.crt")
	tlsKeyPath = filepath.Join(tmpDir, "tls.key")
	tlsCAPath = filepath.Join(tmpDir, "ca.crt")

	t.Cleanup(func() {
		tlsCertPath = oldTLSCertPath
		tlsKeyPath = oldTLSKeyPath
		tlsCAPath = oldTLSCAPath
	})

	return tmpDir
}

func writeTestCACert(t *testing.T, targetPath string) {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "redis-operator-test-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(derBytes)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(targetPath, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	}), 0o600))
}

func generateTestCA(t *testing.T, serial int64, commonName string) (*ecdsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject: pkix.Name{
			CommonName: commonName,
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(derBytes)
	require.NoError(t, err)

	return privateKey, cert, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
}

func generateServerCertSignedByCA(
	t *testing.T,
	serial int64,
	caKey *ecdsa.PrivateKey,
	caCert *x509.Certificate,
) *x509.Certificate {
	t.Helper()

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject: pkix.Name{
			CommonName: "redis-server",
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, caCert, serverKey.Public(), caKey)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(derBytes)
	require.NoError(t, err)
	return cert
}

func TestWriteRedisConf_BasicConfig(t *testing.T) {
	tmpDir := overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, "redis.conf"))
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "port 6379")
	assert.Contains(t, content, "bind 0.0.0.0")
	assert.Contains(t, content, "appendonly yes")
	assert.Contains(t, content, "aof-use-rdb-preamble yes")
	assert.Contains(t, content, "appenddirname appendonlydir")
	assert.Contains(t, content, "save 900 1")
	assert.Contains(t, content, "save 300 10")
	assert.Contains(t, content, "save 60 10000")
	assert.NotContains(t, content, "replicaof")
	assert.NotContains(t, content, "aclfile")
}

func TestWriteRedisConf_WithReplicaOf(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "replicaof 10.0.0.1 6379", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "replicaof 10.0.0.1 6379")
}

func TestWriteRedisConf_WithMasterAuth(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "replicaof 10.0.0.1 6379", "external-password", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "replicaof 10.0.0.1 6379")
	assert.Contains(t, content, "masterauth external-password")
}

func TestWriteRedisConf_WithRedisParams(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"maxmemory":        "256mb",
				"maxmemory-policy": "allkeys-lru",
			},
		},
	}
	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "maxmemory 256mb")
	assert.Contains(t, content, "maxmemory-policy allkeys-lru")
}

func TestWriteRedisConf_WithACLConfig(t *testing.T) {
	tmpDir := overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			ACLConfigSecret: &redisv1.LocalObjectReference{Name: "acl-secret"},
		},
	}
	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	expectedACLPath := filepath.Join(tmpDir, "users.acl")
	assert.Contains(t, content, "aclfile "+expectedACLPath)
}

func TestWriteRedisConf_AllOptions(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Redis: map[string]string{
				"hz": "50",
			},
			ACLConfigSecret: &redisv1.LocalObjectReference{Name: "my-acl"},
		},
	}
	err := writeRedisConf(cluster, "replicaof 10.0.0.5 6379", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "port 6379")
	assert.Contains(t, content, "replicaof 10.0.0.5 6379")
	assert.Contains(t, content, "hz 50")
	assert.Contains(t, content, "aclfile")
}

func TestWriteRedisConf_WithTLS(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}

	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	assert.Contains(t, lines, "tls-port 6379")
	assert.Contains(t, lines, "port 0")
	assert.Contains(t, lines, "tls-cert-file /tls/tls.crt")
	assert.Contains(t, lines, "tls-key-file /tls/tls.key")
	assert.Contains(t, lines, "tls-ca-cert-file /tls/ca.crt")
	assert.Contains(t, lines, "tls-auth-clients optional")
	assert.Contains(t, lines, "tls-replication yes")
	assert.NotContains(t, lines, "port 6379")
}

func TestWriteRedisConf_WithTLSInSentinelMode(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Mode:      redisv1.ClusterModeSentinel,
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}

	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	assert.Contains(t, lines, "port 6379")
	assert.NotContains(t, lines, "tls-port 6379")
	assert.NotContains(t, lines, "port 0")
}

func TestIsTLSEnabled_SentinelModeDisabled(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Mode:      redisv1.ClusterModeSentinel,
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}
	assert.False(t, isTLSEnabled(cluster))
}

func TestValidateTLSMode_SentinelTLSRejected(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Mode:      redisv1.ClusterModeSentinel,
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}
	err := validateTLSMode(cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported in sentinel mode")
}

func TestValidateTLSMode_SentinelSingleTLSReferenceRejected(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			Mode:      redisv1.ClusterModeSentinel,
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
		},
	}
	err := validateTLSMode(cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported in sentinel mode")
}

func TestRedisTLSConfig_Disabled(t *testing.T) {
	cluster := &redisv1.RedisCluster{}

	cfg, err := redisTLSConfig(cluster)
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestRedisTLSConfig_EnabledMissingCA(t *testing.T) {
	overrideTLSPaths(t)

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}

	cfg, err := redisTLSConfig(cluster)
	require.Error(t, err)
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "reading CA certificate")
}

func TestRedisTLSConfig_EnabledWithCA(t *testing.T) {
	tlsDir := overrideTLSPaths(t)
	writeTestCACert(t, filepath.Join(tlsDir, "ca.crt"))

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}

	cfg, err := redisTLSConfig(cluster)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	assert.NotNil(t, cfg.RootCAs)
}

func TestRedisTLSConfig_ReloadsCAOnHandshake(t *testing.T) {
	tlsDir := overrideTLSPaths(t)
	ca1Key, ca1Cert, ca1PEM := generateTestCA(t, 101, "redis-test-ca-1")
	require.NoError(t, os.WriteFile(filepath.Join(tlsDir, "ca.crt"), ca1PEM, 0o600))

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			TLSSecret: &redisv1.LocalObjectReference{Name: "tls-secret"},
			CASecret:  &redisv1.LocalObjectReference{Name: "ca-secret"},
		},
	}

	cfg, err := redisTLSConfig(cluster)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.VerifyConnection)

	certFromCA1 := generateServerCertSignedByCA(t, 201, ca1Key, ca1Cert)
	err = cfg.VerifyConnection(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certFromCA1},
	})
	require.NoError(t, err)

	ca2Key, ca2Cert, ca2PEM := generateTestCA(t, 102, "redis-test-ca-2")
	require.NoError(t, os.WriteFile(filepath.Join(tlsDir, "ca.crt"), ca2PEM, 0o600))

	err = cfg.VerifyConnection(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certFromCA1},
	})
	require.Error(t, err)

	certFromCA2 := generateServerCertSignedByCA(t, 202, ca2Key, ca2Cert)
	err = cfg.VerifyConnection(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certFromCA2},
	})
	require.NoError(t, err)
}

func TestWriteRedisConf_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "nested", "data")

	oldDataDir := dataDir
	oldConfPath := redisConfPath
	dataDir = subDir
	redisConfPath = filepath.Join(subDir, "redis.conf")
	t.Cleanup(func() {
		dataDir = oldDataDir
		redisConfPath = oldConfPath
	})

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	// Verify the nested directory was created.
	info, err := os.Stat(subDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// Verify file was written.
	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	assert.True(t, len(data) > 0)
}

func TestWriteRedisConf_EndsWithNewline(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{}
	err := writeRedisConf(cluster, "", "", "")
	require.NoError(t, err)

	data, err := os.ReadFile(redisConfPath)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(data), "\n"), "config should end with newline")
}

func TestWriteSentinelConf_BasicConfig(t *testing.T) {
	tmpDir := overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-cluster",
		},
	}
	err := writeSentinelConf(cluster, "10.0.0.1", "")
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(tmpDir, "sentinel.conf"))
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "port 26379")
	assert.Contains(t, content, "bind 0.0.0.0")
	assert.Contains(t, content, "dir "+tmpDir)
	assert.Contains(t, content, "sentinel monitor test-cluster 10.0.0.1 6379 2")
	assert.Contains(t, content, "sentinel down-after-milliseconds test-cluster 5000")
	assert.Contains(t, content, "sentinel failover-timeout test-cluster 60000")
	assert.Contains(t, content, "sentinel parallel-syncs test-cluster 1")
	assert.True(t, strings.HasSuffix(content, "\n"), "config should end with newline")
}

func TestWriteSentinelConf_WithAuthPass(t *testing.T) {
	overrideDataDir(t)

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "secure-cluster",
		},
	}
	err := writeSentinelConf(cluster, "10.0.0.2", "s3cr3t")
	require.NoError(t, err)

	data, err := os.ReadFile(sentinelConfPath)
	require.NoError(t, err)
	content := string(data)

	assert.Contains(t, content, "sentinel auth-pass secure-cluster s3cr3t")
}

func TestResolveSentinelAuthPassword_NoneConfigured(t *testing.T) {
	cluster := &redisv1.RedisCluster{}
	password, err := resolveSentinelAuthPassword(cluster)
	require.NoError(t, err)
	assert.Equal(t, "", password)
}

func TestResolveSentinelAuthPassword_FromProjectedSecret(t *testing.T) {
	tmpDir := t.TempDir()
	oldProjected := projectedSecretsDir
	projectedSecretsDir = tmpDir
	t.Cleanup(func() {
		projectedSecretsDir = oldProjected
	})

	secretName := "auth-secret"
	secretDir := filepath.Join(tmpDir, secretName)
	require.NoError(t, os.MkdirAll(secretDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secretDir, "password"), []byte("my-pass\n"), 0o600))

	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{
			AuthSecret: &redisv1.LocalObjectReference{Name: secretName},
		},
	}
	password, err := resolveSentinelAuthPassword(cluster)
	require.NoError(t, err)
	assert.Equal(t, "my-pass", password)
}

func TestGeneratedAuthSecretName(t *testing.T) {
	assert.Equal(t, "demo-auth", generatedAuthSecretName("demo"))
}

func TestEffectiveAuthSecretName(t *testing.T) {
	t.Run("uses explicit spec auth secret", func(t *testing.T) {
		cluster := &redisv1.RedisCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
			Spec: redisv1.RedisClusterSpec{
				AuthSecret: &redisv1.LocalObjectReference{Name: "custom-auth"},
			},
		}
		assert.Equal(t, "custom-auth", effectiveAuthSecretName(cluster))
	})

	t.Run("falls back to generated auth secret", func(t *testing.T) {
		cluster := &redisv1.RedisCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		}
		assert.Equal(t, "demo-auth", effectiveAuthSecretName(cluster))
	})

	t.Run("returns empty when cluster has no name", func(t *testing.T) {
		cluster := &redisv1.RedisCluster{}
		assert.Equal(t, "", effectiveAuthSecretName(cluster))
	})
}

func TestResolveLocalAuthPassword_FromProjectedSecret(t *testing.T) {
	tmpDir := t.TempDir()
	oldProjected := projectedSecretsDir
	projectedSecretsDir = tmpDir
	t.Cleanup(func() {
		projectedSecretsDir = oldProjected
	})

	secretDir := filepath.Join(tmpDir, "demo-auth")
	require.NoError(t, os.MkdirAll(secretDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secretDir, "password"), []byte(" local-pass \n"), 0o600))

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
	}
	password, err := resolveLocalAuthPassword(cluster)
	require.NoError(t, err)
	assert.Equal(t, "local-pass", password)
}

func TestResolveLocalAuthPassword_MissingProjectedSecret(t *testing.T) {
	tmpDir := t.TempDir()
	oldProjected := projectedSecretsDir
	projectedSecretsDir = tmpDir
	t.Cleanup(func() {
		projectedSecretsDir = oldProjected
	})

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
	}
	_, err := resolveLocalAuthPassword(cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading projected auth secret demo-auth/password")
}

func TestSplitBrainGuardLogic(t *testing.T) {
	tests := []struct {
		name           string
		currentPrimary string
		podName        string
		wantIsPrimary  bool
	}{
		{"pod is primary", "cluster-0", "cluster-0", true},
		{"pod is replica", "cluster-0", "cluster-1", false},
		{"empty primary first boot", "", "cluster-0", true},
		{"empty primary any pod", "", "cluster-5", true},
		{"different pods", "test-0", "test-3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This mirrors the logic from Run(): isPrimary := cluster.Status.CurrentPrimary == podName || cluster.Status.CurrentPrimary == ""
			isPrimary := tt.currentPrimary == tt.podName || tt.currentPrimary == ""
			assert.Equal(t, tt.wantIsPrimary, isPrimary)
		})
	}
}

func TestResolvePodIP(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-0", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: "10.244.0.5"},
	}
	c := fake.NewClientBuilder().WithObjects(pod).Build()

	ip, err := resolvePodIP(context.Background(), c, "cluster-0", "default")
	require.NoError(t, err)
	assert.Equal(t, "10.244.0.5", ip)
}

func TestResolvePodIP_DifferentNamespace(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "myredis-2", Namespace: "production"},
		Status:     corev1.PodStatus{PodIP: "10.244.1.9"},
	}
	c := fake.NewClientBuilder().WithObjects(pod).Build()

	ip, err := resolvePodIP(context.Background(), c, "myredis-2", "production")
	require.NoError(t, err)
	assert.Equal(t, "10.244.1.9", ip)
}

func TestResolvePodIP_PodNotFound(t *testing.T) {
	c := fake.NewClientBuilder().Build()

	_, err := resolvePodIP(context.Background(), c, "nonexistent", "default")
	require.Error(t, err)
}

func TestResolvePodIP_NoIPAssigned(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-0", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithObjects(pod).Build()

	_, err := resolvePodIP(context.Background(), c, "cluster-0", "default")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no IP assigned")
}
