// Package run provides the top-level Run function for the instance manager.
package run

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/reconciler"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/replication"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/webserver"
)

var (
	dataDir       = "/data"
	redisConfPath = "/data/redis.conf"
	tlsCertPath   = "/tls/tls.crt"
	tlsKeyPath    = "/tls/tls.key"
	tlsCAPath     = "/tls/ca.crt"
)

const (
	redisPort                      = 6379
	httpListenAddr                 = ":8080"
	defaultPrimaryIsolationTimeout = 5 * time.Second
	defaultReplicaModeSourcePort   = int32(6379)
)

// Run is the top-level entry point for the instance manager.
// It performs the startup sequence documented in internal/instance-manager/run/README.md.
func Run(ctx context.Context, clusterName, podName, namespace string) error {
	logger := log.FromContext(ctx).WithValues("pod", podName, "cluster", clusterName)
	logger.Info("Starting instance manager")

	// Step 1: Create a Kubernetes client.
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("getting kubeconfig: %w", err)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("creating Kubernetes client: %w", err)
	}

	// Step 2: Fetch the RedisCluster CR.
	var cluster redisv1.RedisCluster
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      clusterName,
		Namespace: namespace,
	}, &cluster); err != nil {
		return fmt.Errorf("fetching RedisCluster %s/%s: %w", namespace, clusterName, err)
	}
	if err := validateTLSMode(&cluster); err != nil {
		return err
	}

	// Step 3: Determine role and apply split-brain guard.
	var replicaOfDirective string
	var masterAuth string
	if isReplicaModeEnabled(&cluster) {
		source := cluster.Spec.ReplicaMode.Source
		if source == nil || strings.TrimSpace(source.Host) == "" {
			return fmt.Errorf("replica mode is enabled but source.host is missing")
		}
		sourcePort := replicaModeSourcePort(source)
		replicaOfDirective = fmt.Sprintf("replicaof %s %d", source.Host, sourcePort)
		if source.AuthSecretName != "" {
			password, err := readProjectedSecretPassword(source.AuthSecretName)
			if err != nil {
				return fmt.Errorf("reading replica mode auth secret %s/password: %w", source.AuthSecretName, err)
			}
			masterAuth = password
		}
		logger.Info("Replica mode: starting as external replica", "sourceHost", source.Host, "sourcePort", sourcePort)
	} else {
		isPrimary := cluster.Status.CurrentPrimary == podName || cluster.Status.CurrentPrimary == ""
		if !isPrimary {
			// Unconditionally start as replica of the current primary.
			primaryIP, err := resolvePodIP(ctx, k8sClient, cluster.Status.CurrentPrimary, namespace)
			if err != nil {
				return fmt.Errorf("resolving primary IP for split-brain guard: %w", err)
			}
			replicaOfDirective = fmt.Sprintf("replicaof %s %d", primaryIP, redisPort)
			logger.Info("Split-brain guard: starting as replica", "primary", cluster.Status.CurrentPrimary, "primaryIP", primaryIP)
		} else {
			logger.Info("Starting as primary")
		}
	}

	// Step 4: Write redis.conf.
	if err := writeRedisConf(&cluster, replicaOfDirective, masterAuth); err != nil {
		return fmt.Errorf("writing redis.conf: %w", err)
	}

	// Step 5: Start redis-server as a child process.
	redisCmd := exec.CommandContext(ctx, "redis-server", redisConfPath)
	redisCmd.Stdout = os.Stdout
	redisCmd.Stderr = os.Stderr
	if err := redisCmd.Start(); err != nil {
		return fmt.Errorf("starting redis-server: %w", err)
	}
	logger.Info("redis-server started", "pid", redisCmd.Process.Pid)

	// Step 6: Create Redis client for local instance.
	tlsConfig, err := redisTLSConfig(&cluster)
	if err != nil {
		return fmt.Errorf("building redis client TLS config: %w", err)
	}
	redisClient := redis.NewClient(&redis.Options{
		Addr:      fmt.Sprintf("127.0.0.1:%d", redisPort),
		TLSConfig: tlsConfig,
	})
	defer func() { _ = redisClient.Close() }()

	// Step 7: Start HTTP server (goroutine).
	srv := webserver.NewServer(
		redisClient,
		httpListenAddr,
		func(promCtx context.Context) error {
			return replication.Promote(promCtx, redisClient)
		},
		func(demCtx context.Context, primaryIP string, port int) error {
			return replication.SetReplicaOf(demCtx, redisClient, primaryIP, port)
		},
	)
	srv.SetMetricsIdentity(clusterName, namespace, podName)
	srv.SetPrimaryIsolationConfig(k8sClient, webserver.PrimaryIsolationConfig{
		Enabled:          primaryIsolationEnabled(cluster.Spec.PrimaryIsolation),
		ClusterName:      clusterName,
		Namespace:        namespace,
		PodName:          podName,
		APIServerTimeout: primaryIsolationAPIServerTimeout(cluster.Spec.PrimaryIsolation),
		PeerTimeout:      primaryIsolationPeerTimeout(cluster.Spec.PrimaryIsolation),
	})
	srv.SetRedisCmd(redisCmd)

	go func() {
		if err := srv.Start(ctx); err != nil {
			logger.Error(err, "HTTP server stopped with error")
		}
	}()

	// Step 8: Start the InstanceReconciler watch loop (goroutine).
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				namespace: {},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("creating controller manager for instance reconciler: %w", err)
	}

	instReconciler := reconciler.NewInstanceReconciler(
		mgr.GetClient(),
		redisClient,
		mgr.GetEventRecorderFor("redis-operator-instance"),
		clusterName,
		podName,
		namespace,
	)
	instReconciler.SetRedisCmd(redisCmd)

	if err := instReconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up instance reconciler: %w", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			logger.Error(err, "Instance reconciler manager stopped with error")
		}
	}()

	// Step 9: Block on redis-server wait.
	if err := redisCmd.Wait(); err != nil {
		return fmt.Errorf("redis-server exited: %w", err)
	}

	logger.Info("redis-server exited cleanly")
	return nil
}

// writeRedisConf generates redis.conf from the cluster spec.
func writeRedisConf(cluster *redisv1.RedisCluster, replicaOfDirective, masterAuth string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	var lines []string

	// Base configuration.
	lines = append(lines, "bind 0.0.0.0",
		fmt.Sprintf("dir %s", dataDir),
		"appendonly yes",
		"aof-use-rdb-preamble yes",
		"appenddirname appendonlydir",
		"save 900 1",
		"save 300 10",
		"save 60 10000",
	)
	if isTLSEnabled(cluster) {
		lines = append(lines,
			fmt.Sprintf("tls-port %d", redisPort),
			"port 0",
			fmt.Sprintf("tls-cert-file %s", tlsCertPath),
			fmt.Sprintf("tls-key-file %s", tlsKeyPath),
			fmt.Sprintf("tls-ca-cert-file %s", tlsCAPath),
			"tls-auth-clients optional",
			"tls-replication yes",
		)
	} else {
		lines = append(lines, fmt.Sprintf("port %d", redisPort))
	}

	// Replication directive (split-brain guard).
	if replicaOfDirective != "" {
		lines = append(lines, replicaOfDirective)
	}
	if masterAuth != "" {
		lines = append(lines, fmt.Sprintf("masterauth %s", masterAuth))
	}

	// User-specified redis.conf parameters.
	for key, val := range cluster.Spec.Redis {
		lines = append(lines, fmt.Sprintf("%s %s", key, val))
	}

	// ACL file if configured.
	if cluster.Spec.ACLConfigSecret != nil {
		lines = append(lines, fmt.Sprintf("aclfile %s", filepath.Join(dataDir, "users.acl")))
	}

	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(redisConfPath, []byte(content), 0644)
}

func isReplicaModeEnabled(cluster *redisv1.RedisCluster) bool {
	return cluster != nil && cluster.Spec.ReplicaMode != nil && cluster.Spec.ReplicaMode.Enabled
}

func replicaModeSourcePort(source *redisv1.ReplicaSourceSpec) int32 {
	if source == nil || source.Port <= 0 {
		return defaultReplicaModeSourcePort
	}
	return source.Port
}

func readProjectedSecretPassword(secretName string) (string, error) {
	secretPath := filepath.Join(resolveProjectedSecretsDir(), secretName, "password")
	content, err := os.ReadFile(secretPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(content)), nil
}

func hasTLSSpec(cluster *redisv1.RedisCluster) bool {
	return cluster.Spec.TLSSecret != nil && cluster.Spec.CASecret != nil
}

func hasAnyTLSSpecReference(cluster *redisv1.RedisCluster) bool {
	return cluster.Spec.TLSSecret != nil || cluster.Spec.CASecret != nil
}

func isTLSEnabled(cluster *redisv1.RedisCluster) bool {
	return hasTLSSpec(cluster) && cluster.Spec.Mode != redisv1.ClusterModeSentinel
}

func validateTLSMode(cluster *redisv1.RedisCluster) error {
	if cluster.Spec.Mode == redisv1.ClusterModeSentinel && hasAnyTLSSpecReference(cluster) {
		return fmt.Errorf("TLS is not supported in sentinel mode yet")
	}
	return nil
}

func redisTLSConfig(cluster *redisv1.RedisCluster) (*tls.Config, error) {
	if !isTLSEnabled(cluster) {
		return nil, nil
	}

	rootCAs, err := loadRootCAsFromFile(tlsCAPath)
	if err != nil {
		return nil, err
	}

	// The local client connects via 127.0.0.1, so hostname verification is not
	// meaningful. Verify the certificate chain against the configured CA instead.
	// The CA bundle is re-read on each handshake so projected secret updates are
	// picked up without restarting the process.
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		//nolint:gosec // hostname is intentionally skipped for loopback-only local client
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("redis server did not present a certificate")
			}
			currentRootCAs, err := loadRootCAsFromFile(tlsCAPath)
			if err != nil {
				return fmt.Errorf("reloading CA certificate for Redis TLS verification: %w", err)
			}

			intermediates := x509.NewCertPool()
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}

			_, err = state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         currentRootCAs,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			if err != nil {
				return fmt.Errorf("verifying redis server certificate chain: %w", err)
			}
			return nil
		},
	}, nil
}

func loadRootCAsFromFile(path string) (*x509.CertPool, error) {
	caCert, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading CA certificate %s: %w", path, err)
	}

	rootCAs := x509.NewCertPool()
	if ok := rootCAs.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("parsing CA certificate %s", path)
	}
	return rootCAs, nil
}

func primaryIsolationEnabled(cfg *redisv1.PrimaryIsolationSpec) bool {
	if cfg == nil || cfg.Enabled == nil {
		return true
	}
	return *cfg.Enabled
}

func primaryIsolationAPIServerTimeout(cfg *redisv1.PrimaryIsolationSpec) time.Duration {
	if cfg == nil || cfg.APIServerTimeout == nil || cfg.APIServerTimeout.Duration <= 0 {
		return defaultPrimaryIsolationTimeout
	}
	return cfg.APIServerTimeout.Duration
}

func primaryIsolationPeerTimeout(cfg *redisv1.PrimaryIsolationSpec) time.Duration {
	if cfg == nil || cfg.PeerTimeout == nil || cfg.PeerTimeout.Duration <= 0 {
		return defaultPrimaryIsolationTimeout
	}
	return cfg.PeerTimeout.Duration
}

// resolvePodIP looks up a pod's IP address from the Kubernetes API.
func resolvePodIP(ctx context.Context, c client.Client, podName, namespace string) (string, error) {
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return "", fmt.Errorf("getting pod %s/%s for IP resolution: %w", namespace, podName, err)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s/%s has no IP assigned yet", namespace, podName)
	}
	return pod.Status.PodIP, nil
}
