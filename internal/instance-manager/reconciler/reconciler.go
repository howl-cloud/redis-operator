// Package reconciler provides the in-pod watch loop for the instance manager.
package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/replication"
)

// InstanceReconciler watches the RedisCluster CR from inside the pod.
type InstanceReconciler struct {
	client      client.Client
	redisClient *redis.Client
	recorder    record.EventRecorder
	clusterName string
	podName     string
	namespace   string

	mu       sync.Mutex
	redisCmd *exec.Cmd
}

// NewInstanceReconciler creates a new InstanceReconciler.
func NewInstanceReconciler(
	c client.Client,
	redisClient *redis.Client,
	recorder record.EventRecorder,
	clusterName, podName, namespace string,
) *InstanceReconciler {
	return &InstanceReconciler{
		client:      c,
		redisClient: redisClient,
		recorder:    recorder,
		clusterName: clusterName,
		podName:     podName,
		namespace:   namespace,
	}
}

// SetRedisCmd sets the current redis-server command for fencing control.
func (r *InstanceReconciler) SetRedisCmd(cmd *exec.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.redisCmd = cmd
}

// Reconcile handles each reconciliation cycle.
func (r *InstanceReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("pod", r.podName, "cluster", r.clusterName)

	// Fetch the current RedisCluster CR.
	var cluster redisv1.RedisCluster
	if err := r.client.Get(ctx, types.NamespacedName{
		Name:      r.clusterName,
		Namespace: r.namespace,
	}, &cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("fetching RedisCluster: %w", err)
	}

	// Step 1: Fencing check.
	if r.isFenced(&cluster) {
		logger.Info("Pod is fenced, stopping redis-server")
		r.recorder.Eventf(&cluster, corev1.EventTypeWarning, "InstanceFenced", "Pod %s is fenced, stopping redis-server", r.podName)
		r.stopRedis()
		return reconcile.Result{}, nil
	}

	// Step 2: Role reconciliation.
	if err := r.reconcileRole(ctx, &cluster); err != nil {
		logger.Error(err, "Failed to reconcile role")
		return reconcile.Result{}, err
	}

	// Step 3: Config reconciliation (redis.conf parameters).
	if err := r.reconcileConfig(ctx, &cluster); err != nil {
		logger.Error(err, "Failed to reconcile config")
		// Not fatal -- continue to status reporting.
	} else if len(cluster.Spec.Redis) > 0 {
		r.recorder.Event(&cluster, corev1.EventTypeNormal, "ConfigReloaded", "Redis configuration reloaded")
	}

	// Step 4: Secret reconciliation.
	if err := r.reconcileSecrets(ctx, &cluster); err != nil {
		logger.Error(err, "Failed to reconcile secrets")
		// Not fatal -- continue to status reporting.
	}

	// Step 5: Status reporting.
	if err := r.reportStatus(ctx, &cluster); err != nil {
		logger.Error(err, "Failed to report status")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// SetupWithManager registers this reconciler to watch the RedisCluster CR.
func (r *InstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1.RedisCluster{}).
		Named("instance-reconciler").
		Complete(r)
}

// isFenced checks if this pod is in the fencing annotation list.
func (r *InstanceReconciler) isFenced(cluster *redisv1.RedisCluster) bool {
	fenced, ok := cluster.Annotations[redisv1.FencingAnnotationKey]
	if !ok {
		return false
	}
	var fencedPods []string
	if err := json.Unmarshal([]byte(fenced), &fencedPods); err != nil {
		return false
	}
	for _, p := range fencedPods {
		if p == r.podName {
			return true
		}
	}
	return false
}

// stopRedis kills the redis-server process if running.
func (r *InstanceReconciler) stopRedis() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redisCmd != nil && r.redisCmd.Process != nil {
		_ = r.redisCmd.Process.Signal(os.Interrupt)
	}
}

// reconcileRole ensures this instance has the correct replication role.
func (r *InstanceReconciler) reconcileRole(ctx context.Context, cluster *redisv1.RedisCluster) error {
	info, err := replication.GetInfo(ctx, r.redisClient)
	if err != nil {
		return fmt.Errorf("getting replication info: %w", err)
	}

	isPrimary := cluster.Status.CurrentPrimary == r.podName

	if isPrimary && info.Role == "slave" {
		// Should be primary but is a replica -- promote.
		r.recorder.Eventf(cluster, corev1.EventTypeNormal, "PromotedToPrimary", "Pod %s promoted to primary", r.podName)
		return replication.Promote(ctx, r.redisClient)
	}

	if !isPrimary && info.Role == "master" {
		// Should be a replica but is primary -- demote.
		r.recorder.Eventf(cluster, corev1.EventTypeNormal, "DemotedToReplica", "Pod %s demoted to replica of %s", r.podName, cluster.Status.CurrentPrimary)
		primaryIP, err := r.getPodIP(ctx, cluster.Status.CurrentPrimary, cluster.Namespace)
		if err != nil {
			return fmt.Errorf("resolving primary IP: %w", err)
		}
		return replication.SetReplicaOf(ctx, r.redisClient, primaryIP, 6379)
	}

	return nil
}

// reconcileConfig applies live-reloadable config changes via CONFIG SET.
func (r *InstanceReconciler) reconcileConfig(ctx context.Context, cluster *redisv1.RedisCluster) error {
	for key, val := range cluster.Spec.Redis {
		// Skip config parameters that require restart.
		if requiresRestart(key) {
			continue
		}
		if err := r.redisClient.ConfigSet(ctx, key, val).Err(); err != nil {
			return fmt.Errorf("CONFIG SET %s: %w", key, err)
		}
	}
	return nil
}

// reconcileSecrets handles live secret rotation.
func (r *InstanceReconciler) reconcileSecrets(ctx context.Context, cluster *redisv1.RedisCluster) error {
	// Auth secret: CONFIG SET requirepass.
	if cluster.Spec.AuthSecret != nil {
		password, err := r.readSecretKey(ctx, cluster.Namespace, cluster.Spec.AuthSecret.Name, "password")
		if err != nil {
			return fmt.Errorf("reading auth secret: %w", err)
		}
		if password != "" {
			if err := r.redisClient.ConfigSet(ctx, "requirepass", password).Err(); err != nil {
				return fmt.Errorf("CONFIG SET requirepass: %w", err)
			}
		}
	}

	// ACL config secret: rewrite ACL file and issue ACL LOAD.
	if cluster.Spec.ACLConfigSecret != nil {
		acl, err := r.readSecretKey(ctx, cluster.Namespace, cluster.Spec.ACLConfigSecret.Name, "acl")
		if err != nil {
			return fmt.Errorf("reading ACL secret: %w", err)
		}
		if acl != "" {
			if err := os.WriteFile("/data/users.acl", []byte(acl), 0600); err != nil {
				return fmt.Errorf("writing ACL file: %w", err)
			}
			if err := r.redisClient.Do(ctx, "ACL", "LOAD").Err(); err != nil {
				return fmt.Errorf("ACL LOAD: %w", err)
			}
		}
	}

	return nil
}

// reportStatus patches the instancesStatus map for this pod.
func (r *InstanceReconciler) reportStatus(ctx context.Context, cluster *redisv1.RedisCluster) error {
	info, err := replication.GetInfo(ctx, r.redisClient)
	if err != nil {
		return fmt.Errorf("getting replication info for status: %w", err)
	}

	status := redisv1.InstanceStatus{
		Role:              info.Role,
		Connected:         true,
		ReplicationOffset: info.MasterReplOffset,
		ConnectedReplicas: int32(info.ConnectedReplicas),
		MasterLinkStatus:  info.MasterLinkStatus,
	}
	if info.Role == "slave" {
		status.ReplicationOffset = info.SlaveReplOffset
	}

	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Status.InstancesStatus == nil {
		cluster.Status.InstancesStatus = make(map[string]redisv1.InstanceStatus)
	}
	cluster.Status.InstancesStatus[r.podName] = status

	return r.client.Status().Patch(ctx, cluster, patch)
}

// getPodIP resolves a pod name to its IP address via DNS.
func (r *InstanceReconciler) getPodIP(_ context.Context, podName, namespace string) (string, error) {
	return fmt.Sprintf("%s.%s.svc.cluster.local", podName, namespace), nil
}

// readSecretKey reads a specific key from a Kubernetes Secret.
func (r *InstanceReconciler) readSecretKey(ctx context.Context, namespace, secretName, key string) (string, error) {
	// In production, read from projected volume mount rather than API.
	// Projected volumes are mounted at /projected/<secretName>/<key>.
	data, err := os.ReadFile(fmt.Sprintf("/projected/%s/%s", secretName, key))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading projected secret %s/%s: %w", secretName, key, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// requiresRestart returns true for Redis config keys that need a server restart.
func requiresRestart(key string) bool {
	restartKeys := map[string]bool{
		"bind":             true,
		"port":             true,
		"tls-port":         true,
		"tls-cert-file":    true,
		"tls-key-file":     true,
		"tls-ca-cert-file": true,
		"unixsocket":       true,
		"databases":        true,
	}
	return restartKeys[key]
}
