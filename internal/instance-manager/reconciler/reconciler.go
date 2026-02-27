// Package reconciler provides the in-pod watch loop for the instance manager.
package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

const (
	envProjectedSecretsDir = "REDIS_OPERATOR_PROJECTED_SECRETS_DIR"
	envACLFilePath         = "REDIS_OPERATOR_ACL_FILE_PATH"
	defaultReplicaModePort = 6379
)

var (
	projectedSecretsDir = "/projected"
	usersACLFilePath    = "/data/users.acl"
	tlsCertFilePath     = "/tls/tls.crt"
	tlsKeyFilePath      = "/tls/tls.key"
	tlsCAFilePath       = "/tls/ca.crt"
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

	tlsCertChecksums map[string]string
}

// NewInstanceReconciler creates a new InstanceReconciler.
func NewInstanceReconciler(
	c client.Client,
	redisClient *redis.Client,
	recorder record.EventRecorder,
	clusterName, podName, namespace string,
) *InstanceReconciler {
	return &InstanceReconciler{
		client:           c,
		redisClient:      redisClient,
		recorder:         recorder,
		clusterName:      clusterName,
		podName:          podName,
		namespace:        namespace,
		tlsCertChecksums: map[string]string{},
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

	// Step 5: TLS certificate rotation reconciliation.
	if err := r.reconcileTLSCerts(ctx, &cluster); err != nil {
		logger.Error(err, "Failed to reconcile TLS certificates")
		// Not fatal -- continue to status reporting.
	}

	// Step 6: Status reporting.
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

	if replicaModeEnabled(cluster) {
		sourceHost, sourcePort, err := replicaModeSourceEndpoint(cluster)
		if err != nil {
			return err
		}

		if cluster.Spec.ReplicaMode.Promote && cluster.Status.CurrentPrimary == r.podName {
			if info.Role == "slave" {
				r.recorder.Eventf(cluster, corev1.EventTypeNormal, "ReplicaModePromoteRequested", "Promoting pod %s out of replica mode", r.podName)
				return replication.Promote(ctx, r.redisClient)
			}
			return nil
		}

		if info.Role != "slave" || info.MasterHost != sourceHost || info.MasterPort != sourcePort {
			r.recorder.Eventf(
				cluster,
				corev1.EventTypeNormal,
				"ReplicaModeSourceUpdated",
				"Pod %s configured to replicate from external source %s:%d",
				r.podName,
				sourceHost,
				sourcePort,
			)
			return replication.SetReplicaOf(ctx, r.redisClient, sourceHost, sourcePort)
		}
		return nil
	}

	isPrimary := cluster.Status.CurrentPrimary == r.podName

	if isPrimary && info.Role == "slave" {
		// Should be primary but is a replica -- promote.
		r.recorder.Eventf(cluster, corev1.EventTypeNormal, "PromotedToPrimary", "Pod %s promoted to primary", r.podName)
		return replication.Promote(ctx, r.redisClient)
	}

	if !isPrimary && info.Role == "master" {
		primaryIP, err := r.getPodIP(ctx, cluster.Status.CurrentPrimary, cluster.Namespace)
		if err != nil {
			return fmt.Errorf("resolving primary IP: %w", err)
		}
		r.recorder.Eventf(cluster, corev1.EventTypeNormal, "DemotedToReplica", "Pod %s demoted to replica of %s", r.podName, cluster.Status.CurrentPrimary)
		return replication.SetReplicaOf(ctx, r.redisClient, primaryIP, defaultReplicaModePort)
	}

	if !isPrimary && info.Role == "slave" {
		primaryIP, err := r.getPodIP(ctx, cluster.Status.CurrentPrimary, cluster.Namespace)
		if err != nil {
			return fmt.Errorf("resolving primary IP: %w", err)
		}
		if info.MasterHost != primaryIP || info.MasterPort != defaultReplicaModePort {
			r.recorder.Eventf(
				cluster,
				corev1.EventTypeNormal,
				"ReplicaReconfigured",
				"Pod %s reconfigured to replicate from primary %s",
				r.podName,
				cluster.Status.CurrentPrimary,
			)
			return replication.SetReplicaOf(ctx, r.redisClient, primaryIP, defaultReplicaModePort)
		}
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
	localAuthPassword := ""

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
			if err := r.redisClient.ConfigSet(ctx, "masterauth", password).Err(); err != nil {
				return fmt.Errorf("CONFIG SET masterauth: %w", err)
			}
			localAuthPassword = password
		}
	}

	// ACL config secret: rewrite ACL file and issue ACL LOAD.
	if cluster.Spec.ACLConfigSecret != nil {
		acl, err := r.readSecretKey(ctx, cluster.Namespace, cluster.Spec.ACLConfigSecret.Name, "acl")
		if err != nil {
			return fmt.Errorf("reading ACL secret: %w", err)
		}
		if acl != "" {
			targetACLPath := resolveACLFilePath()
			if err := os.WriteFile(targetACLPath, []byte(acl), 0600); err != nil {
				return fmt.Errorf("writing ACL file: %w", err)
			}
			if err := r.redisClient.Do(ctx, "ACL", "LOAD").Err(); err != nil {
				return fmt.Errorf("ACL LOAD: %w", err)
			}
		}
	}

	// In replica mode, upstream auth may differ from local auth, so override masterauth.
	if replicaModeEnabled(cluster) &&
		cluster.Spec.ReplicaMode.Source != nil &&
		strings.TrimSpace(cluster.Spec.ReplicaMode.Source.AuthSecretName) != "" {
		authSecretName := strings.TrimSpace(cluster.Spec.ReplicaMode.Source.AuthSecretName)
		password, err := r.readSecretKey(ctx, cluster.Namespace, authSecretName, "password")
		if err != nil {
			return fmt.Errorf("reading replica mode auth secret: %w", err)
		}
		if password == "" {
			return fmt.Errorf("replica mode auth secret %s/password is empty or missing", authSecretName)
		}
		if err := r.redisClient.ConfigSet(ctx, "masterauth", password).Err(); err != nil {
			return fmt.Errorf("CONFIG SET masterauth for replica mode: %w", err)
		}
	} else if localAuthPassword == "" {
		// Outside replica mode, avoid stale upstream auth by clearing masterauth.
		if err := r.redisClient.ConfigSet(ctx, "masterauth", "").Err(); err != nil {
			return fmt.Errorf("CONFIG SET masterauth clear: %w", err)
		}
	}

	return nil
}

func (r *InstanceReconciler) reconcileTLSCerts(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if !isTLSEnabled(cluster) {
		r.tlsCertChecksums = map[string]string{}
		return nil
	}

	currentChecksums, err := readTLSCertChecksums()
	if err != nil {
		return err
	}

	if len(r.tlsCertChecksums) == 0 {
		r.tlsCertChecksums = currentChecksums
		return nil
	}

	if checksumsEqual(r.tlsCertChecksums, currentChecksums) {
		return nil
	}

	if err := r.redisClient.ConfigSet(ctx, "tls-cert-file", tlsCertFilePath).Err(); err != nil {
		return fmt.Errorf("CONFIG SET tls-cert-file: %w", err)
	}
	if err := r.redisClient.ConfigSet(ctx, "tls-key-file", tlsKeyFilePath).Err(); err != nil {
		return fmt.Errorf("CONFIG SET tls-key-file: %w", err)
	}
	if err := r.redisClient.ConfigSet(ctx, "tls-ca-cert-file", tlsCAFilePath).Err(); err != nil {
		return fmt.Errorf("CONFIG SET tls-ca-cert-file: %w", err)
	}

	if r.recorder != nil {
		r.recorder.Event(cluster, corev1.EventTypeNormal, "CertificatesRotated", "TLS certificates reloaded")
	}
	r.tlsCertChecksums = currentChecksums
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

// getPodIP resolves a pod name to its IP address from the Kubernetes API.
func (r *InstanceReconciler) getPodIP(ctx context.Context, podName, namespace string) (string, error) {
	var pod corev1.Pod
	if err := r.client.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return "", fmt.Errorf("getting pod %s/%s for IP resolution: %w", namespace, podName, err)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s/%s has no IP assigned yet", namespace, podName)
	}
	return pod.Status.PodIP, nil
}

// readSecretKey reads a specific key from a Kubernetes Secret.
func (r *InstanceReconciler) readSecretKey(ctx context.Context, namespace, secretName, key string) (string, error) {
	// In production, read from projected volume mount rather than API.
	// Projected volumes are mounted at /projected/<secretName>/<key>.
	data, err := os.ReadFile(filepath.Join(resolveProjectedSecretsDir(), secretName, key))
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
		"bind":       true,
		"port":       true,
		"tls-port":   true,
		"unixsocket": true,
		"databases":  true,
	}
	return restartKeys[key]
}

func isTLSEnabled(cluster *redisv1.RedisCluster) bool {
	return cluster.Spec.TLSSecret != nil && cluster.Spec.CASecret != nil
}

func readTLSCertChecksums() (map[string]string, error) {
	checksums := make(map[string]string, 3)
	certFiles := map[string]string{
		"tls-cert-file":    tlsCertFilePath,
		"tls-key-file":     tlsKeyFilePath,
		"tls-ca-cert-file": tlsCAFilePath,
	}

	for key, path := range certFiles {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading TLS certificate file %s: %w", path, err)
		}
		sum := sha256.Sum256(content)
		checksums[key] = hex.EncodeToString(sum[:])
	}

	return checksums, nil
}

func checksumsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if rightValue, ok := right[key]; !ok || rightValue != leftValue {
			return false
		}
	}
	return true
}

func resolveProjectedSecretsDir() string {
	if dir := strings.TrimSpace(os.Getenv(envProjectedSecretsDir)); dir != "" {
		return dir
	}
	return projectedSecretsDir
}

func resolveACLFilePath() string {
	if p := strings.TrimSpace(os.Getenv(envACLFilePath)); p != "" {
		return p
	}
	return usersACLFilePath
}

func replicaModeEnabled(cluster *redisv1.RedisCluster) bool {
	return cluster != nil &&
		cluster.Spec.ReplicaMode != nil &&
		cluster.Spec.ReplicaMode.Enabled
}

func replicaModeSourceEndpoint(cluster *redisv1.RedisCluster) (string, int, error) {
	if cluster == nil || cluster.Spec.ReplicaMode == nil || cluster.Spec.ReplicaMode.Source == nil {
		return "", 0, fmt.Errorf("replica mode source is not configured")
	}
	host := strings.TrimSpace(cluster.Spec.ReplicaMode.Source.Host)
	if host == "" {
		return "", 0, fmt.Errorf("replica mode source host is empty")
	}
	port := int(cluster.Spec.ReplicaMode.Source.Port)
	if port <= 0 {
		port = defaultReplicaModePort
	}
	return host, port, nil
}
