package cluster

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// rollingUpdate performs a rolling update of pods.
// Replicas are updated first (highest ordinal first), primary last via switchover.
func (r *ClusterReconciler) rollingUpdate(ctx context.Context, cluster *redisv1.RedisCluster, desiredHash string) error {
	logger := log.FromContext(ctx)

	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods for rolling update: %w", err)
	}

	var replicas []corev1.Pod
	var primary *corev1.Pod
	for i := range pods {
		if pods[i].Name == cluster.Status.CurrentPrimary {
			primary = &pods[i]
		} else {
			replicas = append(replicas, pods[i])
		}
	}

	r.Recorder.Event(cluster, corev1.EventTypeNormal, "RollingUpdateStarted", "Rolling update started")

	// Sort replicas by ordinal descending (highest first).
	sort.Slice(replicas, func(i, j int) bool {
		return podIndex(cluster.Name, replicas[i].Name) > podIndex(cluster.Name, replicas[j].Name)
	})

	// Update replicas one at a time.
	for _, replica := range replicas {
		currentHash := replica.Labels["redis.io/spec-hash"]
		if currentHash == desiredHash {
			continue
		}

		logger.Info("Rolling update: deleting replica for recreate", "pod", replica.Name)
		if err := r.Delete(ctx, &replica); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting replica %s for update: %w", replica.Name, err)
		}
		// Recreate will happen on next reconcile cycle.
		return nil // One at a time.
	}

	// Update primary last (via switchover).
	if primary != nil {
		currentHash := primary.Labels["redis.io/spec-hash"]
		if currentHash != desiredHash {
			logger.Info("Rolling update: primary needs update, performing switchover", "pod", primary.Name)
			if err := r.switchover(ctx, cluster); err != nil {
				return fmt.Errorf("switchover during rolling update: %w", err)
			}
			// After switchover, the old primary becomes a replica and will be updated
			// on the next reconcile cycle.
			return nil
		}
	}

	r.Recorder.Event(cluster, corev1.EventTypeNormal, "RollingUpdateCompleted", "Rolling update completed")
	return nil
}

// switchover promotes a replica and demotes the current primary.
func (r *ClusterReconciler) switchover(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// Select the best replica (lowest replication lag).
	candidate, err := r.selectFailoverCandidate(ctx, cluster)
	if err != nil {
		return fmt.Errorf("selecting switchover candidate: %w", err)
	}
	if candidate == "" {
		return fmt.Errorf("no suitable replica found for switchover")
	}

	logger.Info("Switchover: promoting replica", "candidate", candidate, "former-primary", cluster.Status.CurrentPrimary)

	// Promote the candidate via HTTP.
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods for switchover: %w", err)
	}

	for _, pod := range pods {
		if pod.Name == candidate && pod.Status.PodIP != "" {
			if err := r.promoteInstance(ctx, pod.Status.PodIP); err != nil {
				return fmt.Errorf("promoting %s: %w", candidate, err)
			}
			break
		}
	}

	return nil
}
