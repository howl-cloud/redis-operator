// Package faults provides reusable chaos fault injection helpers.
package faults

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const pollInterval = 2 * time.Second

// RunKubectl runs kubectl and returns trimmed stdout.
func RunKubectl(ctx context.Context, args ...string) (string, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("running kubectl %q: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(stdout.String()), nil
}

// RunKubectlInNamespace runs kubectl in the provided namespace and returns trimmed stdout.
func RunKubectlInNamespace(ctx context.Context, namespace string, args ...string) (string, error) {
	if namespace == "" {
		return RunKubectl(ctx, args...)
	}

	nsArgs := make([]string, 0, len(args)+2)
	nsArgs = append(nsArgs, "-n", namespace)
	nsArgs = append(nsArgs, args...)
	return RunKubectl(ctx, nsArgs...)
}

// ExecInPod runs a command in a pod via kubectl exec and returns trimmed stdout.
func ExecInPod(ctx context.Context, namespace, podName string, args ...string) (string, error) {
	execArgs := make([]string, 0, len(args)+5)
	execArgs = append(execArgs, "-n", namespace, "exec", podName, "--")
	execArgs = append(execArgs, args...)
	return RunKubectl(ctx, execArgs...)
}

// ExecRedisCLI runs redis-cli in the given pod with REDISCLI_AUTH set.
func ExecRedisCLI(ctx context.Context, namespace, podName, password string, args ...string) (string, error) {
	redisArgs := make([]string, 0, len(args)+4)
	redisArgs = append(redisArgs, "env", "REDISCLI_AUTH="+password, "redis-cli", "--no-auth-warning")
	redisArgs = append(redisArgs, args...)
	return ExecInPod(ctx, namespace, podName, redisArgs...)
}

// KillPod force deletes a pod.
func KillPod(ctx context.Context, c client.Client, namespace, podName string) error {
	gracePeriodSeconds := int64(0)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
	}
	if err := c.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &gracePeriodSeconds}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// KillPodsWithLabels force deletes all pods matching the provided labels.
func KillPodsWithLabels(ctx context.Context, c client.Client, namespace string, labelSet map[string]string) error {
	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels(labelSet)); err != nil {
		return fmt.Errorf("listing pods in %s with labels %v: %w", namespace, labelSet, err)
	}

	for _, pod := range podList.Items {
		if err := KillPod(ctx, c, namespace, pod.Name); err != nil {
			return err
		}
	}
	return nil
}

// WaitForPodReady waits until a pod exists and is Ready.
func WaitForPodReady(ctx context.Context, c client.Client, namespace, podName string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var pod corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for pod %s/%s ready: %w", namespace, podName, err)
	}
	return nil
}

// WaitForPodDeleted waits until the pod is gone.
func WaitForPodDeleted(ctx context.Context, c client.Client, namespace, podName string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var pod corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for pod %s/%s deletion: %w", namespace, podName, err)
	}
	return nil
}

// PodRestartCount returns the highest restart count across all containers.
func PodRestartCount(ctx context.Context, c client.Client, namespace, podName string) (int32, error) {
	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return 0, fmt.Errorf("getting pod %s/%s restart count: %w", namespace, podName, err)
	}

	var restartCount int32
	for _, status := range pod.Status.ContainerStatuses {
		if status.RestartCount > restartCount {
			restartCount = status.RestartCount
		}
	}
	return restartCount, nil
}

// WaitForRestartCountIncrease waits until restart count becomes larger than before.
func WaitForRestartCountIncrease(ctx context.Context, c client.Client, namespace, podName string, before int32, timeout time.Duration) error {
	var baselinePod corev1.Pod
	baselineErr := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &baselinePod)
	if baselineErr != nil {
		return fmt.Errorf("getting pod %s/%s baseline before restart wait: %w", namespace, podName, baselineErr)
	}
	baselineUID := string(baselinePod.UID)
	baselineStart := baselinePod.Status.StartTime

	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var pod corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}

		if string(pod.UID) != baselineUID {
			return true, nil
		}
		if baselineStart != nil && pod.Status.StartTime != nil && pod.Status.StartTime.After(baselineStart.Time) {
			return true, nil
		}

		var restarts int32
		for _, status := range pod.Status.ContainerStatuses {
			if status.RestartCount > restarts {
				restarts = status.RestartCount
			}
		}
		if restarts > before {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for restart count increase for pod %s/%s: %w", namespace, podName, err)
	}
	return nil
}

// ListClusterPods returns pods for the given Redis cluster, sorted by name.
func ListClusterPods(ctx context.Context, c client.Client, namespace, clusterName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{
		redisv1.LabelCluster: clusterName,
	}); err != nil {
		return nil, fmt.Errorf("listing pods for cluster %s/%s: %w", namespace, clusterName, err)
	}

	pods := append([]corev1.Pod(nil), podList.Items...)
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Name < pods[j].Name
	})
	return pods, nil
}

// GetPrimaryPod returns the first pod labeled as the primary.
func GetPrimaryPod(ctx context.Context, c client.Client, namespace, clusterName string) (corev1.Pod, error) {
	var cluster redisv1.RedisCluster
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: clusterName}, &cluster); err == nil {
		if cluster.Status.CurrentPrimary != "" {
			var pod corev1.Pod
			if getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: cluster.Status.CurrentPrimary}, &pod); getErr == nil {
				return pod, nil
			}
		}
	}

	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{
		redisv1.LabelCluster: clusterName,
		redisv1.LabelRole:    redisv1.LabelRolePrimary,
	}); err != nil {
		return corev1.Pod{}, fmt.Errorf("listing primary pod for cluster %s/%s: %w", namespace, clusterName, err)
	}

	if len(podList.Items) == 0 {
		return corev1.Pod{}, fmt.Errorf("no primary pod found for cluster %s/%s", namespace, clusterName)
	}
	sort.Slice(podList.Items, func(i, j int) bool {
		return podList.Items[i].Name < podList.Items[j].Name
	})
	return podList.Items[0], nil
}

// GetReplicaPods returns pods labeled as replicas, sorted by name.
func GetReplicaPods(ctx context.Context, c client.Client, namespace, clusterName string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := c.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabels{
		redisv1.LabelCluster: clusterName,
		redisv1.LabelRole:    redisv1.LabelRoleReplica,
	}); err != nil {
		return nil, fmt.Errorf("listing replica pods for cluster %s/%s: %w", namespace, clusterName, err)
	}

	pods := append([]corev1.Pod(nil), podList.Items...)
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Name > pods[j].Name
	})
	return pods, nil
}

// GetOperatorPod finds an operator pod by standard labels, with a namespace fallback.
func GetOperatorPod(ctx context.Context, c client.Client, operatorNamespace, releaseName string) (corev1.Pod, error) {
	var labeledPods corev1.PodList
	if err := c.List(ctx, &labeledPods, client.InNamespace(operatorNamespace), client.MatchingLabels{
		"app.kubernetes.io/name":     "redis-operator",
		"app.kubernetes.io/instance": releaseName,
	}); err != nil {
		return corev1.Pod{}, fmt.Errorf("listing labeled operator pods in %s: %w", operatorNamespace, err)
	}
	if len(labeledPods.Items) > 0 {
		sort.Slice(labeledPods.Items, func(i, j int) bool {
			return labeledPods.Items[i].Name < labeledPods.Items[j].Name
		})
		return labeledPods.Items[0], nil
	}

	var allPods corev1.PodList
	if err := c.List(ctx, &allPods, client.InNamespace(operatorNamespace)); err != nil {
		return corev1.Pod{}, fmt.Errorf("listing operator namespace pods in %s: %w", operatorNamespace, err)
	}
	if len(allPods.Items) == 0 {
		return corev1.Pod{}, fmt.Errorf("no pods found in operator namespace %s", operatorNamespace)
	}
	sort.Slice(allPods.Items, func(i, j int) bool {
		return allPods.Items[i].Name < allPods.Items[j].Name
	})
	return allPods.Items[0], nil
}

// RestartOperator deletes one operator pod and waits for deployment rollout.
func RestartOperator(ctx context.Context, c client.Client, operatorNamespace, releaseName string, timeout time.Duration) error {
	operatorPod, err := GetOperatorPod(ctx, c, operatorNamespace, releaseName)
	if err != nil {
		return err
	}

	if err := KillPod(ctx, c, operatorNamespace, operatorPod.Name); err != nil {
		return err
	}

	rolloutTimeout := int(timeout.Seconds())
	if rolloutTimeout < 1 {
		rolloutTimeout = 1
	}
	_, err = RunKubectl(ctx, "-n", operatorNamespace, "rollout", "status", "deployment/"+releaseName, fmt.Sprintf("--timeout=%ds", rolloutTimeout))
	if err != nil {
		return fmt.Errorf("waiting for operator rollout in namespace %s: %w", operatorNamespace, err)
	}
	return nil
}

// WaitForPhase waits until status.phase equals the expected value.
func WaitForPhase(ctx context.Context, c client.Reader, namespace, name, expected string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var cluster redisv1.RedisCluster
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cluster); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return string(cluster.Status.Phase) == expected, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for RedisCluster %s/%s phase %q: %w", namespace, name, expected, err)
	}
	return nil
}

// WaitForPrimaryChange waits until status.currentPrimary changes from oldPrimary.
func WaitForPrimaryChange(ctx context.Context, c client.Reader, namespace, name, oldPrimary string, timeout time.Duration) (string, error) {
	var primary string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var cluster redisv1.RedisCluster
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cluster); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		primary = cluster.Status.CurrentPrimary
		return primary != "" && primary != oldPrimary, nil
	})
	if err != nil {
		return "", fmt.Errorf("waiting for RedisCluster %s/%s primary change from %q: %w", namespace, name, oldPrimary, err)
	}
	return primary, nil
}

// WaitForFenceAnnotation waits until the fencing annotation appears.
func WaitForFenceAnnotation(ctx context.Context, c client.Reader, namespace, name string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		var cluster redisv1.RedisCluster
		if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cluster); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		value := strings.TrimSpace(cluster.Annotations[redisv1.FencingAnnotationKey])
		return value != "", nil
	})
	if err != nil {
		return fmt.Errorf("waiting for RedisCluster %s/%s fencing annotation: %w", namespace, name, err)
	}
	return nil
}

// WaitForPodRole waits until a pod reports the desired Redis replication role.
func WaitForPodRole(ctx context.Context, c client.Client, namespace, podName, password, expectedRole string, timeout time.Duration) error {
	var lastRole string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		role, err := PodRedisRole(ctx, c, namespace, podName, password)
		if err != nil {
			lastRole = err.Error()
			return false, nil
		}
		lastRole = role
		return role == expectedRole, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for pod %s/%s role %q, last value %q: %w", namespace, podName, expectedRole, lastRole, err)
	}
	return nil
}

// PodRedisRole reads INFO replication and returns the role.
func PodRedisRole(ctx context.Context, c client.Client, namespace, podName, password string) (string, error) {
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &corev1.Pod{}); err != nil {
		return "", fmt.Errorf("getting pod %s/%s before redis role check: %w", namespace, podName, err)
	}

	info, err := ExecRedisCLI(ctx, namespace, podName, password, "INFO", "replication")
	if err != nil {
		return "", fmt.Errorf("running INFO replication on pod %s/%s: %w", namespace, podName, err)
	}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(line, "role:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "role:")), nil
		}
	}
	return "", errors.New("role not found in INFO replication output")
}

// ReplicationOffset returns master/slave replication offset from INFO replication.
func ReplicationOffset(ctx context.Context, namespace, podName, password string) (int64, error) {
	info, err := ExecRedisCLI(ctx, namespace, podName, password, "INFO", "replication")
	if err != nil {
		return 0, fmt.Errorf("running INFO replication on pod %s/%s: %w", namespace, podName, err)
	}

	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(line, "master_repl_offset:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "master_repl_offset:"))
			var parsed int64
			if _, scanErr := fmt.Sscanf(value, "%d", &parsed); scanErr != nil {
				return 0, fmt.Errorf("parsing master_repl_offset %q: %w", value, scanErr)
			}
			return parsed, nil
		}
		if strings.HasPrefix(line, "slave_repl_offset:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "slave_repl_offset:"))
			var parsed int64
			if _, scanErr := fmt.Sscanf(value, "%d", &parsed); scanErr != nil {
				return 0, fmt.Errorf("parsing slave_repl_offset %q: %w", value, scanErr)
			}
			return parsed, nil
		}
	}

	return 0, errors.New("replication offset not found in INFO replication output")
}
