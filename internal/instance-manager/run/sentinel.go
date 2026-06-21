package run

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/webserver"
)

var sentinelConfPath = "/data/sentinel.conf"

const envProjectedSecretsDir = "REDIS_OPERATOR_PROJECTED_SECRETS_DIR"

// sentinelTLSReloadInterval is how often a TLS-enabled sentinel pod checks its
// projected cert files for changes. Sentinel pods do not run the instance
// reconciler, so this loop provides the live cert-reload parity that data pods
// get from reconcileTLSCerts.
var sentinelTLSReloadInterval = 30 * time.Second

var projectedSecretsDir = "/projected"

// RunSentinel is the top-level entry point for sentinel pods.
func RunSentinel(ctx context.Context, clusterName, podName, namespace string) error {
	logger := log.FromContext(ctx).WithValues("pod", podName, "cluster", clusterName)
	logger.Info("Starting sentinel instance manager")

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

	var cluster redisv1.RedisCluster
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Name:      clusterName,
		Namespace: namespace,
	}, &cluster); err != nil {
		return fmt.Errorf("fetching RedisCluster %s/%s: %w", namespace, clusterName, err)
	}

	if cluster.Status.CurrentPrimary == "" {
		return fmt.Errorf("currentPrimary is not set; cannot start sentinel")
	}

	primaryIP, err := resolvePodIP(ctx, k8sClient, cluster.Status.CurrentPrimary, namespace)
	if err != nil {
		return fmt.Errorf("resolving primary IP for sentinel monitor: %w", err)
	}

	authPass, err := resolveSentinelAuthPassword(&cluster)
	if err != nil {
		return fmt.Errorf("resolving sentinel auth password: %w", err)
	}

	if err := writeSentinelConf(&cluster, primaryIP, authPass); err != nil {
		return fmt.Errorf("writing sentinel.conf: %w", err)
	}

	sentinelCmd := exec.CommandContext(ctx, "redis-sentinel", sentinelConfPath)
	sentinelCmd.Stdout = os.Stdout
	sentinelCmd.Stderr = os.Stderr
	if err := sentinelCmd.Start(); err != nil {
		return fmt.Errorf("starting redis-sentinel: %w", err)
	}
	logger.Info("redis-sentinel started", "pid", sentinelCmd.Process.Pid)

	tlsConfig, err := redisTLSConfig(&cluster)
	if err != nil {
		return fmt.Errorf("building sentinel client TLS config: %w", err)
	}
	sentinelClient := redis.NewClient(&redis.Options{
		Addr:      fmt.Sprintf("127.0.0.1:%d", redisv1.SentinelPort),
		TLSConfig: tlsConfig,
	})
	defer func() { _ = sentinelClient.Close() }()

	srv := webserver.NewSentinelServer(sentinelClient, httpListenAddr, sentinelCmd)
	go func() {
		if err := srv.Start(ctx); err != nil {
			logger.Error(err, "HTTP server stopped with error")
		}
	}()

	if isTLSEnabled(&cluster) {
		go watchSentinelTLSCerts(ctx, sentinelClient)
	}

	if err := sentinelCmd.Wait(); err != nil {
		return fmt.Errorf("redis-sentinel exited: %w", err)
	}

	logger.Info("redis-sentinel exited cleanly")
	return nil
}

func writeSentinelConf(cluster *redisv1.RedisCluster, primaryIP, authPass string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	lines := []string{
		"bind 0.0.0.0",
		fmt.Sprintf("dir %s", dataDir),
	}
	if isTLSEnabled(cluster) {
		// Sentinel listens on the TLS port only (port 0 disables the plaintext
		// listener). tls-replication yes makes sentinel connect to monitored
		// masters and replicas over TLS, matching the data pods' tls-port setup.
		lines = append(lines,
			fmt.Sprintf("tls-port %d", redisv1.SentinelPort),
			"port 0",
			fmt.Sprintf("tls-cert-file %s", tlsCertPath),
			fmt.Sprintf("tls-key-file %s", tlsKeyPath),
			fmt.Sprintf("tls-ca-cert-file %s", tlsCAPath),
			"tls-auth-clients optional",
			"tls-replication yes",
		)
	} else {
		lines = append(lines, fmt.Sprintf("port %d", redisv1.SentinelPort))
	}
	lines = append(lines,
		fmt.Sprintf("sentinel monitor %s %s %d %d", cluster.Name, primaryIP, redisPort, redisv1.SentinelQuorum),
		fmt.Sprintf("sentinel down-after-milliseconds %s 5000", cluster.Name),
		fmt.Sprintf("sentinel failover-timeout %s 60000", cluster.Name),
		fmt.Sprintf("sentinel parallel-syncs %s 1", cluster.Name),
	)
	if authPass != "" {
		lines = append(lines, fmt.Sprintf("sentinel auth-pass %s %s", cluster.Name, authPass))
	}

	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(sentinelConfPath, []byte(content), 0644)
}

func resolveSentinelAuthPassword(cluster *redisv1.RedisCluster) (string, error) {
	if cluster.Spec.AuthSecret == nil {
		return "", nil
	}

	data, err := os.ReadFile(filepath.Join(resolveProjectedSecretsDir(), cluster.Spec.AuthSecret.Name, "password"))
	if err != nil {
		return "", fmt.Errorf("reading projected auth secret %s/password: %w", cluster.Spec.AuthSecret.Name, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func resolveProjectedSecretsDir() string {
	if value := strings.TrimSpace(os.Getenv(envProjectedSecretsDir)); value != "" {
		return value
	}
	return projectedSecretsDir
}

// watchSentinelTLSCerts reloads TLS material into the running redis-sentinel
// process when the projected cert files change. Without it, a cert or CA
// rotation would not take effect until the sentinel pod restarts — and a CA
// rotation in particular would break the operator's TLS verification of the
// sentinel until then. Data pods get the equivalent behavior from the instance
// reconciler, which sentinel pods do not run.
func watchSentinelTLSCerts(ctx context.Context, sentinelClient *redis.Client) {
	logger := log.FromContext(ctx)

	lastSum, err := tlsCertsChecksum()
	if err != nil {
		logger.Error(err, "Reading initial TLS cert checksum for sentinel reload")
	}

	ticker := time.NewTicker(sentinelTLSReloadInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sum, err := tlsCertsChecksum()
			if err != nil {
				logger.Error(err, "Reading TLS cert checksum for sentinel reload")
				continue
			}
			if sum == lastSum {
				continue
			}
			if err := reloadSentinelTLSCerts(ctx, sentinelClient); err != nil {
				logger.Error(err, "Reloading TLS certs into sentinel")
				continue
			}
			lastSum = sum
			logger.Info("Reloaded TLS certificates into sentinel")
		}
	}
}

// tlsCertsChecksum returns a SHA-256 over the concatenated cert, key, and CA
// files so a change to any of them is detected.
func tlsCertsChecksum() (string, error) {
	h := sha256.New()
	for _, path := range []string{tlsCertPath, tlsKeyPath, tlsCAPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading TLS file %s: %w", path, err)
		}
		_, _ = h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// reloadSentinelTLSCerts points redis-sentinel at the (already-updated) cert
// files via CONFIG SET, which reloads them without a restart.
func reloadSentinelTLSCerts(ctx context.Context, sentinelClient *redis.Client) error {
	updates := []struct{ key, value string }{
		{"tls-cert-file", tlsCertPath},
		{"tls-key-file", tlsKeyPath},
		{"tls-ca-cert-file", tlsCAPath},
	}
	for _, u := range updates {
		if err := sentinelClient.ConfigSet(ctx, u.key, u.value).Err(); err != nil {
			return fmt.Errorf("CONFIG SET %s: %w", u.key, err)
		}
	}
	return nil
}
