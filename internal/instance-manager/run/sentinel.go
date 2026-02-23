package run

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

	sentinelClient := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("127.0.0.1:%d", redisv1.SentinelPort),
	})
	defer func() { _ = sentinelClient.Close() }()

	srv := webserver.NewSentinelServer(sentinelClient, httpListenAddr, sentinelCmd)
	go func() {
		if err := srv.Start(ctx); err != nil {
			logger.Error(err, "HTTP server stopped with error")
		}
	}()

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
		fmt.Sprintf("port %d", redisv1.SentinelPort),
		"bind 0.0.0.0",
		fmt.Sprintf("dir %s", dataDir),
		fmt.Sprintf("sentinel monitor %s %s %d %d", cluster.Name, primaryIP, redisPort, redisv1.SentinelQuorum),
		fmt.Sprintf("sentinel down-after-milliseconds %s 5000", cluster.Name),
		fmt.Sprintf("sentinel failover-timeout %s 60000", cluster.Name),
		fmt.Sprintf("sentinel parallel-syncs %s 1", cluster.Name),
	}
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
