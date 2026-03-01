// Package main provides the entry point for the redis-operator binary.
// The binary serves dual roles selected via Cobra subcommand:
//   - `redis-operator controller` — runs the Kubernetes controller-manager
//   - `redis-operator instance` — runs the in-pod instance manager
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/howl-cloud/redis-operator/internal/cmd/manager/controller"
	"github.com/howl-cloud/redis-operator/internal/cmd/manager/restore"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/run"
)

func main() {
	opts := zap.Options{Development: true}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	rootCmd := &cobra.Command{
		Use:   "redis-operator",
		Short: "Redis Kubernetes Operator",
		Long:  "A Kubernetes operator for managing Redis 7.2 clusters.",
	}

	rootCmd.AddCommand(controllerCmd())
	rootCmd.AddCommand(instanceCmd())
	rootCmd.AddCommand(copyBinaryCmd())
	rootCmd.AddCommand(restoreCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func controllerCmd() *cobra.Command {
	var metricsAddr string
	var pprofBindAddr string
	var maxConcurrentReconciles int
	var enableLeaderElection bool
	var enableWebhooks bool

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the Kubernetes controller-manager",
		Long:  "Starts the ctrl.Manager with all reconcilers and webhooks.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()
			return controller.RunController(
				ctx,
				metricsAddr,
				pprofBindAddr,
				maxConcurrentReconciles,
				enableLeaderElection,
				enableWebhooks,
			)
		},
	}

	cmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":9090", "The address the metric endpoint binds to")
	cmd.Flags().StringVar(&pprofBindAddr, "pprof-bind-address", "", "The address the pprof endpoint binds to (empty disables pprof)")
	cmd.Flags().IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 5, "Maximum number of concurrent reconciles for RedisCluster resources")
	cmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager")
	cmd.Flags().BoolVar(&enableWebhooks, "webhook-enabled", true, "Enable admission webhooks for RedisCluster resources")

	return cmd
}

func instanceCmd() *cobra.Command {
	var clusterName string
	var podName string
	var podNamespace string
	var role string

	cmd := &cobra.Command{
		Use:   "instance",
		Short: "Run the in-pod instance manager",
		Long:  "Supervises redis-server or redis-sentinel, and runs HTTP health endpoints inside the pod.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()

			if clusterName == "" {
				clusterName = os.Getenv("CLUSTER_NAME")
			}
			if podName == "" {
				podName = os.Getenv("POD_NAME")
			}
			if podNamespace == "" {
				podNamespace = os.Getenv("POD_NAMESPACE")
			}

			if clusterName == "" || podName == "" || podNamespace == "" {
				return fmt.Errorf("--cluster-name, --pod-name, and --pod-namespace are required (or set CLUSTER_NAME, POD_NAME, POD_NAMESPACE env vars)")
			}

			switch role {
			case "data":
				return run.Run(ctx, clusterName, podName, podNamespace)
			case "sentinel":
				return run.RunSentinel(ctx, clusterName, podName, podNamespace)
			default:
				return fmt.Errorf("invalid --role %q: must be one of data,sentinel", role)
			}
		},
	}

	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "Name of the RedisCluster CR")
	cmd.Flags().StringVar(&podName, "pod-name", "", "Name of this pod")
	cmd.Flags().StringVar(&podNamespace, "pod-namespace", "", "Namespace of this pod")
	cmd.Flags().StringVar(&role, "role", "data", "Role for this instance manager process: data or sentinel")

	return cmd
}

func copyBinaryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "copy-binary <destination>",
		Short: "Copy the manager binary to the given path",
		Long:  "Copies the running binary to <destination>. Used by the copy-manager init container to install the instance manager into a shared emptyDir volume without requiring coreutils.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			src, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolving executable path: %w", err)
			}
			return copyBinary(src, args[0])
		},
	}
}

func restoreCmd() *cobra.Command {
	var clusterName string
	var backupName string
	var backupNamespace string
	var dataDir string

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore Redis data from a RedisBackup",
		Long:  "Downloads a completed RedisBackup object from S3 into the pod data directory.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()

			if backupNamespace == "" {
				backupNamespace = os.Getenv("POD_NAMESPACE")
			}
			if clusterName == "" {
				clusterName = os.Getenv("CLUSTER_NAME")
			}

			if clusterName == "" || backupName == "" || backupNamespace == "" || dataDir == "" {
				return fmt.Errorf("--cluster-name, --backup-name, --backup-namespace, and --data-dir are required (or set CLUSTER_NAME/POD_NAMESPACE env vars)")
			}

			return restore.Run(ctx, clusterName, backupName, backupNamespace, dataDir)
		},
	}

	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "Name of the RedisCluster being restored")
	cmd.Flags().StringVar(&backupName, "backup-name", "", "Name of the RedisBackup CR to restore from")
	cmd.Flags().StringVar(&backupNamespace, "backup-namespace", "", "Namespace of the RedisBackup CR")
	cmd.Flags().StringVar(&dataDir, "data-dir", "/data", "Data directory where dump.rdb should be written")

	return cmd
}

// copyBinary copies the file at src to dst with executable permissions (0o755).
// It checks the error on close so that a full-disk condition is not silently swallowed.
func copyBinary(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source %s: %w", src, err)
	}
	defer in.Close() //nolint:errcheck // read-only; close error carries no information

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("opening destination %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copying binary: %w", err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("flushing destination %s: %w", dst, err)
	}

	return nil
}
