package chaos

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

var benchmarkFailurePattern = regexp.MustCompile(`(?i)(^|[^a-z])(error|err|failed)([^a-z]|$)`)

type backgroundCommand struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan error
}

func scenarioContext() (context.Context, context.CancelFunc) {
	parent := suiteCtx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, defaultScenarioTimeout)
}

func prepareBaseline(ctx context.Context, keyPrefix string, keyCount int) error {
	if err := waitForClusterHealthy(ctx); err != nil {
		return err
	}
	if err := refreshRedisPassword(ctx); err != nil {
		return err
	}
	if err := invariants.FlushClusterData(ctx, k8sClient, testNamespace, clusterName, redisPassword); err != nil {
		return err
	}
	if err := invariants.WriteKeys(ctx, k8sClient, testNamespace, clusterName, redisPassword, keyPrefix, keyCount); err != nil {
		return err
	}

	primaryPod, err := getPrimaryPod(ctx)
	if err != nil {
		return err
	}
	requiredReplicas := 1
	if err := waitForReplicationConverged(ctx, testNamespace, primaryPod.Name, redisPassword, requiredReplicas, 10000, 2*time.Minute); err != nil {
		return err
	}

	return nil
}

func waitForReplicationConverged(
	ctx context.Context,
	namespace, primaryPod, password string,
	requiredReplicas, waitTimeoutMS int,
	timeout time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for {
		err := invariants.AssertReplicationConverged(ctx, namespace, primaryPod, password, requiredReplicas, waitTimeoutMS)
		if err == nil {
			return nil
		}
		lastErr = err

		if time.Now().After(deadline) {
			return fmt.Errorf("waiting for replication convergence: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func getPrimaryPod(ctx context.Context) (corev1.Pod, error) {
	return faults.GetPrimaryPod(ctx, k8sClient, testNamespace, clusterName)
}

func getAnyClusterPod(ctx context.Context) (corev1.Pod, error) {
	pods, err := faults.ListClusterPods(ctx, k8sClient, testNamespace, clusterName)
	if err != nil {
		return corev1.Pod{}, err
	}
	if len(pods) == 0 {
		return corev1.Pod{}, fmt.Errorf("no pods found for cluster %s/%s", testNamespace, clusterName)
	}
	return pods[0], nil
}

func getPeerIPsExcluding(ctx context.Context, excludedPodName string) ([]string, error) {
	pods, err := faults.ListClusterPods(ctx, k8sClient, testNamespace, clusterName)
	if err != nil {
		return nil, err
	}
	ips := make([]string, 0, len(pods))
	for _, pod := range pods {
		if pod.Name == excludedPodName || strings.TrimSpace(pod.Status.PodIP) == "" {
			continue
		}
		ips = append(ips, pod.Status.PodIP)
	}
	sort.Strings(ips)
	return ips, nil
}

func getKubernetesAPITargetIPs(ctx context.Context) ([]string, error) {
	var svc corev1.Service
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, &svc)
	if err != nil {
		return nil, fmt.Errorf("getting kubernetes service IP: %w", err)
	}

	targets := make([]string, 0, 4)
	if clusterIP := strings.TrimSpace(svc.Spec.ClusterIP); clusterIP != "" {
		targets = append(targets, clusterIP)
	}

	var endpoints corev1.Endpoints
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, &endpoints); err != nil {
		return nil, fmt.Errorf("getting kubernetes service endpoints: %w", err)
	}
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if ip := strings.TrimSpace(address.IP); ip != "" {
				targets = append(targets, ip)
			}
		}
	}

	sort.Strings(targets)
	deduped := targets[:0]
	for _, target := range targets {
		if len(deduped) == 0 || deduped[len(deduped)-1] != target {
			deduped = append(deduped, target)
		}
	}

	if len(deduped) == 0 {
		return nil, fmt.Errorf("kubernetes API service has no target IPs")
	}
	return append([]string(nil), deduped...), nil
}

func warmPrimaryIsolationPeerCache(ctx context.Context, namespace, podName string) error {
	localPort, err := reserveLocalPort()
	if err != nil {
		return err
	}

	portForwardCtx, portForwardCancel := context.WithCancel(ctx)
	bg, err := startKubectlBackgroundCommand(
		portForwardCtx,
		"-n", namespace,
		"port-forward",
		"pod/"+podName,
		fmt.Sprintf("%d:8080", localPort),
	)
	if err != nil {
		portForwardCancel()
		return err
	}
	defer func() {
		portForwardCancel()
		select {
		case <-bg.done:
		case <-time.After(2 * time.Second):
		}
	}()

	if err := bg.ensureRunning(500 * time.Millisecond); err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 2 * time.Second}
	healthzURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", localPort)

	var lastErr error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, healthzURL, nil)
		if reqErr != nil {
			return fmt.Errorf("creating healthz request to %q: %w", healthzURL, reqErr)
		}

		resp, doErr := httpClient.Do(req)
		if doErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("healthz status %d", resp.StatusCode)
		} else {
			lastErr = doErr
		}

		if err := bg.ensureRunning(100 * time.Millisecond); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for healthz response")
	}
	return fmt.Errorf("warming primary isolation peer cache for pod %s/%s: %w", namespace, podName, lastErr)
}

func reserveLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserving local port: %w", err)
	}
	defer func() {
		_ = listener.Close()
	}()

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("resolving reserved TCP address")
	}
	return tcpAddr.Port, nil
}

func ensureLeaderEndpointExcludes(ctx context.Context, excludedPodName string) error {
	var endpoints corev1.Endpoints
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: clusterName + "-leader"}, &endpoints); err != nil {
		return fmt.Errorf("getting leader endpoints for cluster %s/%s: %w", testNamespace, clusterName, err)
	}

	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.TargetRef != nil && address.TargetRef.Name == excludedPodName {
				return fmt.Errorf("leader service still routes to %s", excludedPodName)
			}
		}
	}
	return nil
}

func performManualRollingRestart(ctx context.Context, workload *backgroundCommand) error {
	replicas, err := faults.GetReplicaPods(ctx, k8sClient, testNamespace, clusterName)
	if err != nil {
		return err
	}

	for _, replica := range replicas {
		if workload != nil {
			if err := workload.ensureRunning(500 * time.Millisecond); err != nil {
				return err
			}
		}
		if err := faults.KillPod(ctx, k8sClient, testNamespace, replica.Name); err != nil {
			return err
		}
		if err := faults.WaitForPodReady(ctx, k8sClient, testNamespace, replica.Name, defaultPodReadyTimeout); err != nil {
			return err
		}
		if err := faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, string(redisv1.ClusterPhaseHealthy), defaultClusterReadyTimeout); err != nil {
			return err
		}
	}

	currentPrimary, err := getPrimaryPod(ctx)
	if err != nil {
		return err
	}
	if workload != nil {
		if err := workload.ensureRunning(500 * time.Millisecond); err != nil {
			return err
		}
	}
	if err := faults.KillPod(ctx, k8sClient, testNamespace, currentPrimary.Name); err != nil {
		return err
	}
	if err := faults.WaitForPodReady(ctx, k8sClient, testNamespace, currentPrimary.Name, defaultPodReadyTimeout); err != nil {
		return err
	}
	if err := faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, string(redisv1.ClusterPhaseHealthy), defaultClusterReadyTimeout); err != nil {
		return err
	}
	return nil
}

func startKubectlBackgroundCommand(ctx context.Context, args ...string) (*backgroundCommand, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	bg := &backgroundCommand{
		cmd:  cmd,
		done: make(chan error, 1),
	}
	cmd.Stdout = &bg.stdout
	cmd.Stderr = &bg.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting kubectl %q: %w", strings.Join(args, " "), err)
	}

	go func() {
		bg.done <- cmd.Wait()
	}()
	return bg, nil
}

func (bg *backgroundCommand) ensureRunning(waitFor time.Duration) error {
	select {
	case err := <-bg.done:
		return fmt.Errorf("background command exited too early: %w; output: %s", err, bg.output())
	case <-time.After(waitFor):
		return nil
	}
}

func (bg *backgroundCommand) wait() (string, error) {
	err := <-bg.done
	out := bg.output()
	if err != nil {
		return out, fmt.Errorf("background command failed: %w; output: %s", err, out)
	}
	return out, nil
}

func (bg *backgroundCommand) output() string {
	return strings.TrimSpace(bg.stdout.String() + "\n" + bg.stderr.String())
}
