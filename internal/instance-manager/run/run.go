// Package run provides the top-level Run function for the instance manager.
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
	"github.com/howl-cloud/redis-operator/internal/instance-manager/reconciler"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/replication"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/webserver"
)

var (
	dataDir       = "/data"
	redisConfPath = "/data/redis.conf"
)

const (
	redisPort      = 6379
	httpListenAddr = ":8080"
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

	// Step 3: Determine role and apply split-brain guard.
	isPrimary := cluster.Status.CurrentPrimary == podName || cluster.Status.CurrentPrimary == ""
	var replicaOfDirective string
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

	// Step 4: Write redis.conf.
	if err := writeRedisConf(&cluster, replicaOfDirective); err != nil {
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
	redisClient := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("127.0.0.1:%d", redisPort),
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
	srv.SetRedisCmd(redisCmd)

	go func() {
		if err := srv.Start(ctx); err != nil {
			logger.Error(err, "HTTP server stopped with error")
		}
	}()

	// Step 8: Start the InstanceReconciler watch loop (goroutine).
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
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
func writeRedisConf(cluster *redisv1.RedisCluster, replicaOfDirective string) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}

	var lines []string

	// Base configuration.
	lines = append(lines,
		fmt.Sprintf("port %d", redisPort),
		"bind 0.0.0.0",
		fmt.Sprintf("dir %s", dataDir),
		"appendonly yes",
		"save 900 1",
		"save 300 10",
		"save 60 10000",
	)

	// Replication directive (split-brain guard).
	if replicaOfDirective != "" {
		lines = append(lines, replicaOfDirective)
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

// resolvePodIP resolves a pod name to its cluster IP via DNS.
func resolvePodIP(_ context.Context, _ client.Client, podName, namespace string) (string, error) {
	return fmt.Sprintf("%s.%s.svc.cluster.local", podName, namespace), nil
}
