package faults

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultNetworkBlockerImage is used for iptables-based network fault injection.
	DefaultNetworkBlockerImage = "nicolaka/netshoot:v0.13"
	defaultNetworkFaultTimeout = 2 * time.Minute
)

// NetworkBlockConfig configures operator -> pod:8080 blocking.
type NetworkBlockConfig struct {
	Namespace string
	PodName   string
	HostNode  string
	SourceIP  string
	TargetIP  string
	Image     string
	ReadyWait time.Duration
}

// PrimaryIsolationConfig configures full primary isolation (API + peer web endpoints).
type PrimaryIsolationConfig struct {
	Namespace   string
	PodName     string
	HostNode    string
	SourceIP    string
	APIServerIP string
	PeerIPs     []string
	Image       string
	ReadyWait   time.Duration
}

// ApplyNetworkBlock creates a privileged hostNetwork pod that drops operator->primary status traffic.
func ApplyNetworkBlock(ctx context.Context, c client.Client, cfg NetworkBlockConfig) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}

	podName := cfg.PodName
	if podName == "" {
		podName = "redis-chaos-netblock"
	}
	image := cfg.Image
	if image == "" {
		image = DefaultNetworkBlockerImage
	}

	ruleScript := fmt.Sprintf(`cleanup() {
  iptables -D FORWARD -s %s -d %s -p tcp --dport 8080 -j DROP 2>/dev/null || true
  iptables -D OUTPUT -s %s -d %s -p tcp --dport 8080 -j DROP 2>/dev/null || true
}
trap cleanup EXIT TERM INT
iptables -I FORWARD 1 -s %s -d %s -p tcp --dport 8080 -j DROP
iptables -I OUTPUT 1 -s %s -d %s -p tcp --dport 8080 -j DROP
while true; do sleep 60 & wait $! || break; done
`, cfg.SourceIP, cfg.TargetIP, cfg.SourceIP, cfg.TargetIP, cfg.SourceIP, cfg.TargetIP, cfg.SourceIP, cfg.TargetIP)

	pod := newNetworkBlockerPod(cfg.Namespace, podName, cfg.HostNode, image, ruleScript)
	if err := createOrReplaceFaultPod(ctx, c, pod, readyWait(cfg.ReadyWait)); err != nil {
		return "", err
	}
	return podName, nil
}

// ApplyPrimaryIsolation blocks a primary from API server and all peer web endpoints.
func ApplyPrimaryIsolation(ctx context.Context, c client.Client, cfg PrimaryIsolationConfig) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}

	podName := cfg.PodName
	if podName == "" {
		podName = "redis-chaos-primary-isolation-netblock"
	}
	image := cfg.Image
	if image == "" {
		image = DefaultNetworkBlockerImage
	}

	var addRules strings.Builder
	var delRules strings.Builder
	for _, peerIP := range cfg.PeerIPs {
		peerIP = strings.TrimSpace(peerIP)
		if peerIP == "" {
			continue
		}
		_, _ = fmt.Fprintf(&addRules, "iptables -I FORWARD 1 -s %s -d %s -p tcp --dport 8080 -j DROP\n", cfg.SourceIP, peerIP)
		_, _ = fmt.Fprintf(&addRules, "iptables -I OUTPUT 1 -s %s -d %s -p tcp --dport 8080 -j DROP\n", cfg.SourceIP, peerIP)

		_, _ = fmt.Fprintf(&delRules, "iptables -D FORWARD -s %s -d %s -p tcp --dport 8080 -j DROP 2>/dev/null || true\n", cfg.SourceIP, peerIP)
		_, _ = fmt.Fprintf(&delRules, "iptables -D OUTPUT -s %s -d %s -p tcp --dport 8080 -j DROP 2>/dev/null || true\n", cfg.SourceIP, peerIP)
	}

	apiRules := fmt.Sprintf(`iptables -I FORWARD 1 -s %s -d %s -p tcp --dport 443 -j DROP
iptables -I OUTPUT 1 -s %s -d %s -p tcp --dport 443 -j DROP
iptables -I FORWARD 1 -s %s -d %s -p tcp --dport 6443 -j DROP
iptables -I OUTPUT 1 -s %s -d %s -p tcp --dport 6443 -j DROP
`, cfg.SourceIP, cfg.APIServerIP, cfg.SourceIP, cfg.APIServerIP, cfg.SourceIP, cfg.APIServerIP, cfg.SourceIP, cfg.APIServerIP)

	apiCleanup := fmt.Sprintf(`iptables -D FORWARD -s %s -d %s -p tcp --dport 443 -j DROP 2>/dev/null || true
iptables -D OUTPUT -s %s -d %s -p tcp --dport 443 -j DROP 2>/dev/null || true
iptables -D FORWARD -s %s -d %s -p tcp --dport 6443 -j DROP 2>/dev/null || true
iptables -D OUTPUT -s %s -d %s -p tcp --dport 6443 -j DROP 2>/dev/null || true
`, cfg.SourceIP, cfg.APIServerIP, cfg.SourceIP, cfg.APIServerIP, cfg.SourceIP, cfg.APIServerIP, cfg.SourceIP, cfg.APIServerIP)

	ruleScript := fmt.Sprintf(`cleanup() {
%s
%s
}
trap cleanup EXIT TERM INT
%s
%s
while true; do sleep 60 & wait $! || break; done
`, strings.TrimSpace(delRules.String()), strings.TrimSpace(apiCleanup), strings.TrimSpace(addRules.String()), strings.TrimSpace(apiRules))

	pod := newNetworkBlockerPod(cfg.Namespace, podName, cfg.HostNode, image, ruleScript)
	if err := createOrReplaceFaultPod(ctx, c, pod, readyWait(cfg.ReadyWait)); err != nil {
		return "", err
	}
	return podName, nil
}

// RemoveNetworkBlocker removes a previously created network blocker pod.
func RemoveNetworkBlocker(ctx context.Context, c client.Client, namespace, podName string) error {
	if strings.TrimSpace(podName) == "" {
		return nil
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace}}
	if err := c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting network blocker pod %s/%s: %w", namespace, podName, err)
	}
	if err := WaitForPodDeleted(ctx, c, namespace, podName, defaultNetworkFaultTimeout); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func (cfg NetworkBlockConfig) validate() error {
	switch {
	case strings.TrimSpace(cfg.Namespace) == "":
		return fmt.Errorf("network block config: namespace is required")
	case strings.TrimSpace(cfg.HostNode) == "":
		return fmt.Errorf("network block config: host node is required")
	case strings.TrimSpace(cfg.SourceIP) == "":
		return fmt.Errorf("network block config: source IP is required")
	case strings.TrimSpace(cfg.TargetIP) == "":
		return fmt.Errorf("network block config: target IP is required")
	default:
		return nil
	}
}

func (cfg PrimaryIsolationConfig) validate() error {
	switch {
	case strings.TrimSpace(cfg.Namespace) == "":
		return fmt.Errorf("primary isolation config: namespace is required")
	case strings.TrimSpace(cfg.HostNode) == "":
		return fmt.Errorf("primary isolation config: host node is required")
	case strings.TrimSpace(cfg.SourceIP) == "":
		return fmt.Errorf("primary isolation config: source IP is required")
	case strings.TrimSpace(cfg.APIServerIP) == "":
		return fmt.Errorf("primary isolation config: API server IP is required")
	default:
		return nil
	}
}

func readyWait(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultNetworkFaultTimeout
	}
	return timeout
}

func createOrReplaceFaultPod(ctx context.Context, c client.Client, pod *corev1.Pod, waitTimeout time.Duration) error {
	existing := &corev1.Pod{}
	err := c.Get(ctx, client.ObjectKeyFromObject(pod), existing)
	if err == nil {
		if delErr := c.Delete(ctx, existing); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting existing network fault pod %s/%s: %w", pod.Namespace, pod.Name, delErr)
		}
		if waitErr := WaitForPodDeleted(ctx, c, pod.Namespace, pod.Name, waitTimeout); waitErr != nil {
			return waitErr
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking existing network fault pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	if err := c.Create(ctx, pod); err != nil {
		return fmt.Errorf("creating network fault pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	if err := WaitForPodReady(ctx, c, pod.Namespace, pod.Name, waitTimeout); err != nil {
		return err
	}
	return nil
}

func newNetworkBlockerPod(namespace, podName, hostNode, image, script string) *corev1.Pod {
	gracePeriod := int64(15)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "redis-chaos-netblock",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &gracePeriod,
			NodeName:                      hostNode,
			HostNetwork:                   true,
			HostPID:                       true,
			Tolerations: []corev1.Toleration{
				{
					Operator: corev1.TolerationOpExists,
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "blocker",
					Image:   image,
					Command: []string{"/bin/sh", "-cu", script},
					SecurityContext: &corev1.SecurityContext{
						Privileged: ptrTo(true),
					},
				},
			},
		},
	}
}

func ptrTo[T any](v T) *T {
	return &v
}
