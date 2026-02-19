package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// setFence adds a pod name to the fencing annotation on the RedisCluster.
func (r *ClusterReconciler) setFence(ctx context.Context, cluster *redisv1.RedisCluster, podName string) error {
	logger := log.FromContext(ctx)
	logger.Info("Setting fence", "pod", podName)

	fenced := r.getFencedPods(cluster)
	for _, p := range fenced {
		if p == podName {
			return nil // Already fenced.
		}
	}
	fenced = append(fenced, podName)

	if err := r.setFencedPods(ctx, cluster, fenced); err != nil {
		return err
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "FencingSet", "Pod %s has been fenced", podName)
	return nil
}

// clearFence removes a pod name from the fencing annotation.
func (r *ClusterReconciler) clearFence(ctx context.Context, cluster *redisv1.RedisCluster, podName string) error {
	logger := log.FromContext(ctx)
	logger.Info("Clearing fence", "pod", podName)

	fenced := r.getFencedPods(cluster)
	var remaining []string
	for _, p := range fenced {
		if p != podName {
			remaining = append(remaining, p)
		}
	}

	if err := r.setFencedPods(ctx, cluster, remaining); err != nil {
		return err
	}
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "FencingCleared", "Fence cleared for pod %s", podName)
	return nil
}

// getFencedPods returns the list of currently fenced pod names.
func (r *ClusterReconciler) getFencedPods(cluster *redisv1.RedisCluster) []string {
	val, ok := cluster.Annotations[redisv1.FencingAnnotationKey]
	if !ok || val == "" {
		return nil
	}
	var pods []string
	if err := json.Unmarshal([]byte(val), &pods); err != nil {
		return nil
	}
	return pods
}

// setFencedPods writes the fencing annotation on the cluster.
func (r *ClusterReconciler) setFencedPods(ctx context.Context, cluster *redisv1.RedisCluster, pods []string) error {
	patch := client.MergeFrom(cluster.DeepCopy())
	if cluster.Annotations == nil {
		cluster.Annotations = make(map[string]string)
	}

	if len(pods) == 0 {
		delete(cluster.Annotations, redisv1.FencingAnnotationKey)
	} else {
		data, err := json.Marshal(pods)
		if err != nil {
			return fmt.Errorf("marshaling fenced pods: %w", err)
		}
		cluster.Annotations[redisv1.FencingAnnotationKey] = string(data)
	}

	return r.Patch(ctx, cluster, patch)
}

// failover performs a fencing-first failover sequence.
func (r *ClusterReconciler) failover(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)
	formerPrimary := cluster.Status.CurrentPrimary

	r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "FailoverStarted", "Failover initiated, former primary: %s", formerPrimary)

	// Step 1: Fence the former primary first.
	if err := r.setFence(ctx, cluster, formerPrimary); err != nil {
		return fmt.Errorf("fencing former primary %s: %w", formerPrimary, err)
	}

	// Step 2: Select the best failover candidate (lowest lag replica).
	candidate, err := r.selectFailoverCandidate(ctx, cluster)
	if err != nil {
		return fmt.Errorf("selecting failover candidate: %w", err)
	}
	if candidate == "" {
		return fmt.Errorf("no suitable failover candidate found")
	}

	logger.Info("Failover: promoting candidate", "candidate", candidate, "former-primary", formerPrimary)

	// Step 3: Promote the candidate via HTTP.
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return fmt.Errorf("listing pods for failover: %w", err)
	}

	for _, pod := range pods {
		if pod.Name == candidate && pod.Status.PodIP != "" {
			if err := r.promoteInstance(ctx, pod.Status.PodIP); err != nil {
				return fmt.Errorf("promoting %s: %w", candidate, err)
			}
			break
		}
	}

	// Step 4: Update leader service selector and status.
	statusPatch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.CurrentPrimary = candidate
	cluster.Status.Phase = redisv1.ClusterPhaseFailingOver
	if err := r.Status().Patch(ctx, cluster, statusPatch); err != nil {
		return fmt.Errorf("updating status after failover: %w", err)
	}

	// Step 5: Update -leader Service selector.
	if err := r.updateLeaderServiceSelector(ctx, cluster); err != nil {
		return fmt.Errorf("updating leader service after failover: %w", err)
	}

	// Step 6: Clear fence on former primary.
	if err := r.clearFence(ctx, cluster, formerPrimary); err != nil {
		return fmt.Errorf("clearing fence on %s: %w", formerPrimary, err)
	}

	logger.Info("Failover completed", "new-primary", candidate, "former-primary", formerPrimary)
	r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "FailoverCompleted", "Failover completed: new primary is %s (former: %s)", candidate, formerPrimary)
	return nil
}

// selectFailoverCandidate selects the replica with the smallest replication lag.
func (r *ClusterReconciler) selectFailoverCandidate(_ context.Context, cluster *redisv1.RedisCluster) (string, error) {
	var bestCandidate string
	var bestOffset int64 = -1

	for name, status := range cluster.Status.InstancesStatus {
		if name == cluster.Status.CurrentPrimary {
			continue
		}
		if !status.Connected {
			continue
		}
		if status.ReplicationOffset > bestOffset {
			bestOffset = status.ReplicationOffset
			bestCandidate = name
		}
	}

	return bestCandidate, nil
}

// promoteInstance sends POST /v1/promote to a pod's instance manager.
func (r *ClusterReconciler) promoteInstance(ctx context.Context, podIP string) error {
	url := fmt.Sprintf("http://%s:8080/v1/promote", podIP)
	httpClient := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("creating promote request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s returned %d", url, resp.StatusCode)
	}

	return nil
}
