package cluster

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// redisDefaultUser is the implicit Redis ACL user used in published connection URLs.
const redisDefaultUser = "default"

// redisDataPort is the Redis client port exposed by all data Services.
const redisDataPort = 6379

// connectionSecretLabel marks Secrets created as connection projections so the
// operator can find and prune its own published Secrets. It is internal
// bookkeeping, not part of the integration contract.
const connectionSecretLabel = "redis.io/connection-secret"

// reconcileConnectionSecret publishes and maintains the optional connection
// Secret described by spec.connectionSecret. The Secret is a pure projection of
// the cluster's Services and resolved auth password, re-rendered on every
// reconcile so auth rotation and service changes propagate automatically. It is
// owned by the RedisCluster (garbage-collected on delete) and will never
// overwrite a pre-existing Secret the operator does not own.
//
// When the feature is disabled (or the name changes), any previously published
// Secret is deleted so credential-bearing projections are never left behind.
func (r *ClusterReconciler) reconcileConnectionSecret(ctx context.Context, cluster *redisv1.RedisCluster) error {
	desiredName := ""
	if cluster.Spec.ConnectionSecret != nil {
		desiredName = strings.TrimSpace(cluster.Spec.ConnectionSecret.Name)
	}

	if desiredName == "" {
		// Feature disabled (or never enabled): remove any owned projection.
		return r.pruneConnectionSecrets(ctx, cluster, "")
	}

	if err := r.ensureConnectionSecret(ctx, cluster, desiredName); err != nil {
		return err
	}

	// Clean up a previously published Secret if the configured name changed.
	return r.pruneConnectionSecrets(ctx, cluster, desiredName)
}

// ensureConnectionSecret creates or updates the connection Secret named by the
// spec, refusing to overwrite a Secret the operator does not own.
func (r *ClusterReconciler) ensureConnectionSecret(ctx context.Context, cluster *redisv1.RedisCluster, name string) error {
	password, err := r.resolveAuthPassword(ctx, cluster)
	if err != nil {
		return fmt.Errorf("resolving auth password for connection secret: %w", err)
	}

	data := buildConnectionSecretData(cluster, password)
	labels := map[string]string{
		redisv1.LabelCluster:  cluster.Name,
		connectionSecretLabel: "true",
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := controllerutil.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting connection secret owner reference: %w", err)
	}

	var existing corev1.Secret
	key := types.NamespacedName{Name: name, Namespace: cluster.Namespace}
	if err := r.Get(ctx, key, &existing); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("getting connection secret %s: %w", name, err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating connection secret %s: %w", name, err)
		}
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ConnectionSecretPublished",
			"Published connection secret %s", name)
		return nil
	}

	// Collision guard: never overwrite a Secret the operator does not own. This
	// protects user-managed Secrets that happen to share the configured name.
	if !metav1.IsControlledBy(&existing, cluster) {
		r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "ConnectionSecretConflict",
			"Secret %s already exists and is not owned by this RedisCluster; refusing to overwrite", name)
		return nil
	}

	if existing.Type == corev1.SecretTypeOpaque &&
		maps.EqualFunc(existing.Data, data, bytes.Equal) &&
		existing.Labels[connectionSecretLabel] == "true" {
		return nil
	}

	existing.Data = data
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[redisv1.LabelCluster] = cluster.Name
	existing.Labels[connectionSecretLabel] = "true"
	// Update (not Patch): the operator's secrets RBAC grants update, and we hold
	// the full object fetched above.
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("updating connection secret %s: %w", name, err)
	}
	return nil
}

// pruneConnectionSecrets deletes connection Secrets owned by this cluster whose
// name is not keepName. With keepName="" it removes all of them (feature
// disabled).
//
// Identification keys off the controller owner reference, not the marker label:
// the connection Secret is the only Secret the operator owns (the auth Secret is
// intentionally unowned), so an owner-controlled Secret is unambiguously a
// connection projection. Keying off ownership — rather than requiring the marker
// — means Secrets published before the marker label existed are still cleaned up.
// User-managed Secrets are never touched, as they carry no owner reference to the
// cluster. If the operator ever owns other Secret kinds in this namespace, this
// must be narrowed.
func (r *ClusterReconciler) pruneConnectionSecrets(ctx context.Context, cluster *redisv1.RedisCluster, keepName string) error {
	var list corev1.SecretList
	if err := r.List(ctx, &list,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{redisv1.LabelCluster: cluster.Name},
	); err != nil {
		return fmt.Errorf("listing connection secrets: %w", err)
	}

	for i := range list.Items {
		secret := &list.Items[i]
		if secret.Name == keepName || !metav1.IsControlledBy(secret, cluster) {
			continue
		}
		if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting stale connection secret %s: %w", secret.Name, err)
		}
		r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "ConnectionSecretRemoved",
			"Removed connection secret %s", secret.Name)
	}
	return nil
}

// resolveAuthPassword reads the Redis password from the cluster's effective auth
// Secret. It returns an empty string (no error) when no auth Secret is
// configured; a missing or unreadable Secret is an error so reconciliation
// requeues rather than publishing a passwordless URL.
func (r *ClusterReconciler) resolveAuthPassword(ctx context.Context, cluster *redisv1.RedisCluster) (string, error) {
	name := effectiveAuthSecretName(cluster)
	if name == "" {
		return "", nil
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, &secret); err != nil {
		return "", fmt.Errorf("getting auth secret %s: %w", name, err)
	}
	return string(secret.Data["password"]), nil
}

// buildConnectionSecretData renders the connection Secret payload for a cluster.
// It is pure (no I/O) so it can be table-tested across modes. Keys follow the
// snake_case Redis convention documented in docs/connection-secret.md. When a
// password is present it is embedded in every *_url value (and the password key).
func buildConnectionSecretData(cluster *redisv1.RedisCluster, password string) map[string][]byte {
	ns := cluster.Namespace
	scheme := "redis"
	if isTLSEnabled(cluster) {
		scheme = "rediss"
	}
	port := strconv.Itoa(redisDataPort)

	data := map[string]string{
		"cluster_name": cluster.Name,
		"namespace":    ns,
		"mode":         string(cluster.Spec.Mode),
		"port":         port,
		"username":     redisDefaultUser,
	}
	if password != "" {
		data["password"] = password
	}

	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		// Redis Cluster shards data; clients discover the topology via the
		// headless Service. There is no single leader/replica endpoint.
		host := serviceFQDN(clusterHeadlessServiceName(cluster.Name), ns)
		data["host"] = host
		data["url"] = redisURL(scheme, password, host, port)
	} else {
		leaderHost := serviceFQDN(leaderServiceName(cluster.Name), ns)
		replicaHost := serviceFQDN(replicaServiceName(cluster.Name), ns)
		data["host"] = leaderHost
		data["url"] = redisURL(scheme, password, leaderHost, port)
		data["leader_host"] = leaderHost
		data["leader_url"] = redisURL(scheme, password, leaderHost, port)
		data["replica_host"] = replicaHost
		data["replica_url"] = redisURL(scheme, password, replicaHost, port)

		if cluster.Spec.Mode == redisv1.ClusterModeSentinel {
			data["sentinel_host"] = serviceFQDN(sentinelServiceName(cluster.Name), ns)
			data["sentinel_port"] = strconv.Itoa(redisv1.SentinelPort)
			// The Sentinel master name is the RedisCluster name (see sentinel.go).
			data["master_name"] = cluster.Name
		}
	}

	out := make(map[string][]byte, len(data))
	for k, v := range data {
		out[k] = []byte(v)
	}
	return out
}

// serviceFQDN returns the namespace-qualified DNS name for an in-cluster Service,
// resolvable from any namespace (matching the operator's PKI SAN convention).
func serviceFQDN(service, namespace string) string {
	return fmt.Sprintf("%s.%s.svc", service, namespace)
}

// redisURL builds a redis:// (or rediss://) URL, embedding credentials only when
// a password is set. The password is escaped via url.UserPassword.
func redisURL(scheme, password, host, port string) string {
	u := url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, port),
	}
	if password != "" {
		u.User = url.UserPassword(redisDefaultUser, password)
	}
	return u.String()
}
