package chaos

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/chaos/faults"
	"github.com/howl-cloud/redis-operator/test/chaos/invariants"
)

const (
	defaultClusterReadyTimeout = 6 * time.Minute
	defaultPodReadyTimeout     = 5 * time.Minute
	defaultScenarioTimeout     = 15 * time.Minute
)

var (
	k8sClient client.Client

	suiteCtx    context.Context
	suiteCancel context.CancelFunc

	testNamespace       string
	operatorNamespace   string
	releaseName         string
	clusterName         string
	networkBlockerImage string
	redisInstances      int
	redisPassword       string
)

func TestChaos(t *testing.T) {
	if os.Getenv("RUN_CHAOS_TESTS") != "1" {
		t.Skip("skipping chaos suite; set RUN_CHAOS_TESTS=1 to run")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "Redis Operator Chaos Suite")
}

var _ = BeforeSuite(func() {
	testNamespace = envOrDefault("TEST_NS", "default")
	operatorNamespace = envOrDefault("OPERATOR_NS", "redis-operator-system")
	releaseName = envOrDefault("RELEASE_NAME", "redis-operator")
	clusterName = envOrDefault("REDIS_CLUSTER_NAME", "chaos-redis")
	networkBlockerImage = envOrDefault("NETWORK_BLOCKER_IMAGE", faults.DefaultNetworkBlockerImage)
	redisInstances = envIntOrDefault("REDIS_INSTANCES", 3)
	if redisInstances < 1 {
		redisInstances = 1
	}

	cfg, err := loadKubeConfig()
	Expect(err).NotTo(HaveOccurred(), "loading kubeconfig")

	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(redisv1.AddToScheme(scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred(), "creating kubernetes client")

	suiteCtx, suiteCancel = context.WithCancel(context.Background())

	Expect(ensureNamespace(suiteCtx, k8sClient, testNamespace)).To(Succeed())
	Expect(recreateRedisCluster(suiteCtx, k8sClient, testNamespace, clusterName, redisInstances)).To(Succeed())
	Expect(waitForClusterHealthy(suiteCtx)).To(Succeed())

	Expect(refreshRedisPassword(suiteCtx)).To(Succeed())
	Eventually(func() error {
		return invariants.AssertNoSplitBrain(suiteCtx, k8sClient, testNamespace, clusterName, redisPassword)
	}, 2*time.Minute, 3*time.Second).Should(Succeed())
})

var _ = AfterSuite(func() {
	if suiteCancel != nil {
		suiteCancel()
	}
	if k8sClient == nil {
		return
	}

	_ = deleteRedisCluster(context.Background(), k8sClient, testNamespace, clusterName)
	if testNamespace != "default" && testNamespace != "kube-system" {
		_ = deleteNamespace(context.Background(), k8sClient, testNamespace)
	}
})

func loadKubeConfig() (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	if err == nil {
		return cfg, nil
	}

	inClusterCfg, inClusterErr := rest.InClusterConfig()
	if inClusterErr != nil {
		return nil, fmt.Errorf("building kubeconfig from file: %w; in-cluster fallback: %v", err, inClusterErr)
	}
	return inClusterCfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func ensureNamespace(ctx context.Context, c client.Client, namespace string) error {
	var ns corev1.Namespace
	err := c.Get(ctx, types.NamespacedName{Name: namespace}, &ns)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting namespace %s: %w", namespace, err)
	}

	ns = corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}
	if err := c.Create(ctx, &ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating namespace %s: %w", namespace, err)
	}
	return nil
}

func recreateRedisCluster(ctx context.Context, c client.Client, namespace, name string, instances int) error {
	if err := deleteRedisCluster(ctx, c, namespace, name); err != nil {
		return err
	}

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: int32(instances),
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
		},
	}

	if err := c.Create(ctx, cluster); err != nil {
		return fmt.Errorf("creating RedisCluster %s/%s: %w", namespace, name, err)
	}
	return nil
}

func deleteRedisCluster(ctx context.Context, c client.Client, namespace, name string) error {
	cluster := &redisv1.RedisCluster{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cluster)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting RedisCluster %s/%s for deletion: %w", namespace, name, err)
	}

	if err := c.Delete(ctx, cluster); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting RedisCluster %s/%s: %w", namespace, name, err)
	}

	waitErr := wait.PollUntilContextTimeout(ctx, 2*time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var current redisv1.RedisCluster
		getErr := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &current)
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		return false, client.IgnoreNotFound(getErr)
	})
	if waitErr != nil {
		return fmt.Errorf("waiting for RedisCluster %s/%s deletion: %w", namespace, name, waitErr)
	}
	return nil
}

func deleteNamespace(ctx context.Context, c client.Client, namespace string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", namespace, err)
	}
	return nil
}

func waitForClusterHealthy(ctx context.Context) error {
	if err := faults.WaitForPhase(ctx, k8sClient, testNamespace, clusterName, string(redisv1.ClusterPhaseHealthy), defaultClusterReadyTimeout); err != nil {
		return err
	}
	for ordinal := 0; ordinal < redisInstances; ordinal++ {
		podName := fmt.Sprintf("%s-%d", clusterName, ordinal)
		if err := faults.WaitForPodReady(ctx, k8sClient, testNamespace, podName, defaultPodReadyTimeout); err != nil {
			return err
		}
	}
	return nil
}

func refreshRedisPassword(ctx context.Context) error {
	var secret corev1.Secret
	secretName := clusterName + "-auth"
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: secretName}, &secret); err != nil {
		return fmt.Errorf("getting Redis password secret %s/%s: %w", testNamespace, secretName, err)
	}

	password, ok := secret.Data["password"]
	if !ok || len(password) == 0 {
		return fmt.Errorf("secret %s/%s has empty password", testNamespace, secretName)
	}
	redisPassword = string(password)
	return nil
}
