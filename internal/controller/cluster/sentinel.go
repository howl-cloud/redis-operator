package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

var querySentinelMasterFn = querySentinelMaster

// reconcileSentinelMaster syncs currentPrimary and service selectors with sentinel's elected master.
func (r *ClusterReconciler) reconcileSentinelMaster(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if cluster.Spec.Mode != redisv1.ClusterModeSentinel {
		return nil
	}

	sentinelPods, err := r.listSentinelPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing sentinel pods: %w", err)
	}

	tlsConfig, err := r.sentinelClientTLSConfig(ctx, cluster)
	if err != nil {
		return fmt.Errorf("building sentinel client TLS config: %w", err)
	}

	logger := log.FromContext(ctx)
	var masterHost string
	var queryErrors []string
	attempted := 0

	for _, pod := range sentinelPods {
		if pod.Status.PodIP == "" {
			continue
		}
		attempted++
		sentinelAddr := fmt.Sprintf("%s:%d", pod.Status.PodIP, redisv1.SentinelPort)

		host, _, queryErr := querySentinelMasterFn(ctx, sentinelAddr, cluster.Name, tlsConfig)
		if queryErr != nil {
			logger.Error(queryErr, "Failed to query sentinel master", "pod", pod.Name, "addr", sentinelAddr)
			queryErrors = append(queryErrors, fmt.Sprintf("%s: %v", sentinelAddr, queryErr))
			continue
		}
		masterHost = host
		break
	}
	if attempted == 0 {
		return nil
	}
	if masterHost == "" {
		return fmt.Errorf("failed querying sentinel master from %d endpoints: %s", attempted, strings.Join(queryErrors, "; "))
	}

	dataPods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing data pods: %w", err)
	}

	newPrimary := findPrimaryPodBySentinelHost(dataPods, cluster.Namespace, masterHost)
	if newPrimary == "" {
		return fmt.Errorf("sentinel reported master %q but no matching data pod found", masterHost)
	}
	if newPrimary == cluster.Status.CurrentPrimary {
		return nil
	}

	oldPrimary := cluster.Status.CurrentPrimary
	logger.Info("Sentinel reported primary change", "oldPrimary", oldPrimary, "newPrimary", newPrimary, "masterHost", masterHost)

	statusPatch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.CurrentPrimary = newPrimary
	if err := r.Status().Patch(ctx, cluster, statusPatch); err != nil {
		return fmt.Errorf("patching currentPrimary from sentinel: %w", err)
	}

	if err := r.updateLeaderServiceSelector(ctx, cluster); err != nil {
		return fmt.Errorf("updating leader service selector from sentinel: %w", err)
	}

	if err := r.updateDataPodRoleLabels(ctx, cluster, oldPrimary, newPrimary); err != nil {
		return fmt.Errorf("updating pod role labels from sentinel: %w", err)
	}

	r.Recorder.Eventf(
		cluster,
		corev1.EventTypeNormal,
		"SentinelFailover",
		"Sentinel promoted %s (former primary: %s)",
		newPrimary,
		oldPrimary,
	)
	return nil
}

func findPrimaryPodBySentinelHost(dataPods []corev1.Pod, namespace, host string) string {
	for _, pod := range dataPods {
		if pod.Status.PodIP == host {
			return pod.Name
		}
		if fmt.Sprintf("%s.%s.svc.cluster.local", pod.Name, namespace) == host {
			return pod.Name
		}
	}
	return ""
}

func (r *ClusterReconciler) updateDataPodRoleLabels(ctx context.Context, cluster *redisv1.RedisCluster, oldPrimary, newPrimary string) error {
	pods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing data pods for role label update: %w", err)
	}

	for i := range pods {
		pod := &pods[i]
		currentRole := pod.Labels[redisv1.LabelRole]
		desiredRole := currentRole

		if pod.Name == newPrimary {
			desiredRole = redisv1.LabelRolePrimary
		} else if pod.Name == oldPrimary || currentRole == redisv1.LabelRolePrimary {
			desiredRole = redisv1.LabelRoleReplica
		}

		if desiredRole == currentRole {
			continue
		}

		patch := client.MergeFrom(pod.DeepCopy())
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		pod.Labels[redisv1.LabelRole] = desiredRole
		if err := r.Patch(ctx, pod, patch); err != nil {
			return fmt.Errorf("patching role label for pod %s: %w", pod.Name, err)
		}
	}
	return nil
}

// sentinelClientTLSConfig builds the TLS config the operator uses to query
// sentinel pods. It returns nil when the cluster is not TLS-enabled.
//
// The operator dials sentinel pod IPs, which are not present in the server
// certificate SANs, so hostname verification is skipped in favor of explicit
// chain verification against the cluster's CA — mirroring the loopback client
// in the instance manager.
func (r *ClusterReconciler) sentinelClientTLSConfig(ctx context.Context, cluster *redisv1.RedisCluster) (*tls.Config, error) {
	if cluster.Spec.CASecret == nil || cluster.Spec.TLSSecret == nil {
		return nil, nil
	}

	var caSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cluster.Spec.CASecret.Name,
		Namespace: cluster.Namespace,
	}, &caSecret); err != nil {
		return nil, fmt.Errorf("getting CA secret %s: %w", cluster.Spec.CASecret.Name, err)
	}

	caPEM, ok := caSecret.Data["ca.crt"]
	if !ok || len(caPEM) == 0 {
		return nil, fmt.Errorf("CA secret %s is missing key ca.crt", cluster.Spec.CASecret.Name)
	}

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parsing ca.crt from secret %s", cluster.Spec.CASecret.Name)
	}

	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		//nolint:gosec // hostname is intentionally skipped for pod-IP dialing; chain is verified below
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("sentinel did not present a certificate")
			}

			intermediates := x509.NewCertPool()
			for _, cert := range state.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}

			if _, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         rootCAs,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); err != nil {
				return fmt.Errorf("verifying sentinel server certificate chain: %w", err)
			}
			return nil
		},
	}, nil
}

func querySentinelMaster(ctx context.Context, sentinelAddr, masterName string, tlsConfig *tls.Config) (host string, port int, err error) {
	redisClient := redis.NewClient(&redis.Options{
		Addr:      sentinelAddr,
		TLSConfig: tlsConfig,
	})
	defer func() { _ = redisClient.Close() }()

	resp, err := redisClient.Do(ctx, "SENTINEL", "get-master-addr-by-name", masterName).Result()
	if err != nil {
		return "", 0, err
	}

	values, ok := resp.([]interface{})
	if !ok || len(values) != 2 {
		return "", 0, fmt.Errorf("unexpected sentinel response type %T: %v", resp, resp)
	}

	host, err = sentinelValueToString(values[0])
	if err != nil {
		return "", 0, fmt.Errorf("decoding sentinel host: %w", err)
	}
	portStr, err := sentinelValueToString(values[1])
	if err != nil {
		return "", 0, fmt.Errorf("decoding sentinel port: %w", err)
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("parsing sentinel port %q: %w", portStr, err)
	}

	return host, port, nil
}

func sentinelValueToString(v interface{}) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case []byte:
		return string(val), nil
	default:
		return "", fmt.Errorf("unexpected value type %T", v)
	}
}
