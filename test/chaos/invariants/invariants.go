// Package invariants verifies safety and correctness properties after injected faults.
package invariants

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/chaos/faults"
)

// FlushClusterData clears all keys on the current primary through the leader service.
func FlushClusterData(ctx context.Context, c client.Client, namespace, clusterName, password string) error {
	_, err := execRedisLeader(ctx, c, namespace, clusterName, password, "FLUSHALL")
	if err != nil {
		return fmt.Errorf("flushing cluster data for %s/%s: %w", namespace, clusterName, err)
	}
	return nil
}

// WriteKeys writes deterministic key/value pairs using the leader service.
func WriteKeys(ctx context.Context, c client.Client, namespace, clusterName, password, prefix string, count int) error {
	clientPod, err := getAnyClusterPod(ctx, c, namespace, clusterName)
	if err != nil {
		return err
	}

	script := fmt.Sprintf(`i=1
while [ $i -le %d ]; do
  redis-cli --no-auth-warning -h %s-leader SET %s:$i value-$i >/dev/null
  i=$((i+1))
done
`, count, clusterName, prefix)

	_, err = faults.ExecInPod(ctx, namespace, clientPod.Name, "env", "REDISCLI_AUTH="+password, "sh", "-ceu", script)
	if err != nil {
		return fmt.Errorf("writing %d keys with prefix %q using pod %s/%s: %w", count, prefix, namespace, clientPod.Name, err)
	}
	return nil
}

// CountKeys returns the number of keys matching prefix:* on the leader service.
func CountKeys(ctx context.Context, c client.Client, namespace, clusterName, password, prefix string) (int, error) {
	clientPod, err := getAnyClusterPod(ctx, c, namespace, clusterName)
	if err != nil {
		return 0, err
	}

	out, err := faults.ExecInPod(ctx, namespace, clientPod.Name, "env", "REDISCLI_AUTH="+password, "sh", "-ceu",
		fmt.Sprintf("redis-cli --no-auth-warning -h %s-leader --scan --pattern '%s:*' | wc -l", clusterName, prefix))
	if err != nil {
		return 0, fmt.Errorf("counting keys with prefix %q using pod %s/%s: %w", prefix, namespace, clientPod.Name, err)
	}

	parsed, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parsing key count %q for prefix %q: %w", out, prefix, err)
	}
	return parsed, nil
}

// AssertDataIntegrity verifies expected key count and sample values.
func AssertDataIntegrity(ctx context.Context, c client.Client, namespace, clusterName, password, prefix string, expectedCount int) error {
	actualCount, err := CountKeys(ctx, c, namespace, clusterName, password, prefix)
	if err != nil {
		return err
	}
	if actualCount != expectedCount {
		return fmt.Errorf("expected %d keys with prefix %q, got %d", expectedCount, prefix, actualCount)
	}

	if expectedCount == 0 {
		return nil
	}

	firstValue, err := execRedisLeader(ctx, c, namespace, clusterName, password, "GET", fmt.Sprintf("%s:1", prefix))
	if err != nil {
		return err
	}
	lastValue, err := execRedisLeader(ctx, c, namespace, clusterName, password, "GET", fmt.Sprintf("%s:%d", prefix, expectedCount))
	if err != nil {
		return err
	}
	if strings.TrimSpace(firstValue) != "value-1" {
		return fmt.Errorf("unexpected value for %s:1, got %q", prefix, firstValue)
	}
	if strings.TrimSpace(lastValue) != fmt.Sprintf("value-%d", expectedCount) {
		return fmt.Errorf("unexpected value for %s:%d, got %q", prefix, expectedCount, lastValue)
	}

	return nil
}

// AssertReplicationConverged verifies WAIT acknowledges at least required replicas.
func AssertReplicationConverged(ctx context.Context, namespace, primaryPod, password string, requiredReplicas, timeoutMS int) error {
	out, err := faults.ExecRedisCLI(ctx, namespace, primaryPod, password, "WAIT", strconv.Itoa(requiredReplicas), strconv.Itoa(timeoutMS))
	if err != nil {
		return fmt.Errorf("running WAIT %d %d on %s/%s: %w", requiredReplicas, timeoutMS, namespace, primaryPod, err)
	}

	acked, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("parsing WAIT output %q on pod %s/%s: %w", out, namespace, primaryPod, err)
	}
	if acked < requiredReplicas {
		return fmt.Errorf("WAIT %d %d returned %d", requiredReplicas, timeoutMS, acked)
	}
	return nil
}

// AssertOffsetNotRegressed ensures replication offset did not go backwards.
func AssertOffsetNotRegressed(before, after int64) error {
	if after < before {
		return fmt.Errorf("replication offset regressed: before=%d after=%d", before, after)
	}
	return nil
}

// AssertPodNotWritable verifies a pod rejects direct writes.
func AssertPodNotWritable(ctx context.Context, namespace, podName, password, key string) error {
	out, err := faults.ExecRedisCLI(ctx, namespace, podName, password, "SET", key, "blocked")
	if err != nil {
		return nil
	}
	if strings.TrimSpace(out) == "OK" {
		return fmt.Errorf("isolated pod %s/%s accepted write to %q", namespace, podName, key)
	}
	return nil
}

// AssertNoSplitBrain verifies exactly one primary label, one Redis master role, and matching status.currentPrimary.
func AssertNoSplitBrain(ctx context.Context, c client.Client, namespace, clusterName, password string) error {
	var labeledPrimaryPods corev1.PodList
	if err := c.List(ctx, &labeledPrimaryPods, client.InNamespace(namespace), client.MatchingLabels{
		redisv1.LabelCluster: clusterName,
		redisv1.LabelRole:    redisv1.LabelRolePrimary,
	}); err != nil {
		return fmt.Errorf("listing labeled primary pods for %s/%s: %w", namespace, clusterName, err)
	}

	if len(labeledPrimaryPods.Items) != 1 {
		return fmt.Errorf("expected exactly one pod labeled primary for %s/%s, found %d", namespace, clusterName, len(labeledPrimaryPods.Items))
	}

	clusterPods, err := faults.ListClusterPods(ctx, c, namespace, clusterName)
	if err != nil {
		return err
	}
	masterCount := 0
	for _, pod := range clusterPods {
		role, roleErr := faults.PodRedisRole(ctx, c, namespace, pod.Name, password)
		if roleErr != nil {
			return roleErr
		}
		if role == "master" {
			masterCount++
		}
	}
	if masterCount != 1 {
		return fmt.Errorf("expected exactly one Redis master role for %s/%s, found %d", namespace, clusterName, masterCount)
	}

	var cluster redisv1.RedisCluster
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: clusterName}, &cluster); err != nil {
		return fmt.Errorf("getting RedisCluster %s/%s for status.currentPrimary check: %w", namespace, clusterName, err)
	}
	if strings.TrimSpace(cluster.Status.CurrentPrimary) == "" {
		return fmt.Errorf("RedisCluster %s/%s status.currentPrimary is empty", namespace, clusterName)
	}

	role, err := faults.PodRedisRole(ctx, c, namespace, cluster.Status.CurrentPrimary, password)
	if err != nil {
		return err
	}
	if role != "master" {
		return fmt.Errorf("status.currentPrimary %s is not master (role=%q)", cluster.Status.CurrentPrimary, role)
	}

	if cluster.Status.CurrentPrimary != labeledPrimaryPods.Items[0].Name {
		return fmt.Errorf("status.currentPrimary %s does not match labeled primary %s", cluster.Status.CurrentPrimary, labeledPrimaryPods.Items[0].Name)
	}
	return nil
}

func getAnyClusterPod(ctx context.Context, c client.Client, namespace, clusterName string) (corev1.Pod, error) {
	pods, err := faults.ListClusterPods(ctx, c, namespace, clusterName)
	if err != nil {
		return corev1.Pod{}, err
	}
	if len(pods) == 0 {
		return corev1.Pod{}, fmt.Errorf("no pods found for cluster %s/%s", namespace, clusterName)
	}
	return pods[0], nil
}

func execRedisLeader(ctx context.Context, c client.Client, namespace, clusterName, password string, args ...string) (string, error) {
	clientPod, err := getAnyClusterPod(ctx, c, namespace, clusterName)
	if err != nil {
		return "", err
	}
	redisArgs := make([]string, 0, len(args)+7)
	redisArgs = append(redisArgs, "env", "REDISCLI_AUTH="+password, "redis-cli", "--no-auth-warning", "-h", clusterName+"-leader")
	redisArgs = append(redisArgs, args...)
	out, err := faults.ExecInPod(ctx, namespace, clientPod.Name, redisArgs...)
	if err != nil {
		return "", fmt.Errorf("running redis leader command %v from pod %s/%s: %w", args, namespace, clientPod.Name, err)
	}
	return strings.TrimSpace(out), nil
}
