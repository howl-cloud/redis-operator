// Package e2e contains end-to-end tests for the redis-operator.
//
// Tests use controller-runtime's envtest, which starts a real Kubernetes API
// server and etcd binary but does NOT run kubelets. Pods and PVCs are created
// as API objects but never actually schedule or run. Tests therefore assert on
// Kubernetes object creation, spec correctness, and status transitions driven
// by the reconciler — not on actual pod readiness or network connectivity.
//
// Prerequisites:
//   - envtest binaries installed via: setup-envtest use <k8s-version> --bin-dir $(LOCALBIN)
//   - KUBEBUILDER_ASSETS env var pointing to the binaries directory, or
//     rely on setup-envtest auto-discovery (default behaviour of envtest.Environment).
package e2e

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/rand"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/internal/controller/backup"
	"github.com/howl-cloud/redis-operator/internal/controller/cluster"
	"github.com/howl-cloud/redis-operator/webhooks"

	schemeruntime "k8s.io/apimachinery/pkg/runtime"
)

// Package-level variables shared by all test files in this package.
var (
	cfg        *rest.Config
	k8sClient  client.Client
	testEnv    *envtest.Environment
	cancelFunc context.CancelFunc
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Redis Operator E2E Suite")
}

var _ = BeforeSuite(func() {
	ctrl.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	// Locate the CRD manifests relative to this file.
	// __file__ resolves to the directory of suite_test.go at compile time.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	crdPaths := []string{
		filepath.Join(repoRoot, "config", "crd", "bases"),
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     crdPaths,
		ErrorIfCRDPathMissing: true,
	}

	// Allow tests to run even without envtest binaries: skip gracefully.
	// If KUBEBUILDER_ASSETS is unset and binaries are absent, Start() will fail
	// and we skip the whole suite rather than panic.
	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		// Check if this looks like a missing-binary error.
		if os.IsNotExist(err) || isEnvtestBinaryMissing(err) {
			Skip("envtest binaries not found; run 'make setup-envtest' first. Error: " + err.Error())
		}
		Expect(err).NotTo(HaveOccurred(), "starting envtest")
	}

	// Build scheme.
	scheme := schemeruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))

	// Create a direct client for assertions.
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	// Create manager without leader election (not needed in tests).
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable metrics in tests
		},
		HealthProbeBindAddress: "0", // disable health probes in tests
		LeaderElection:         false,
	})
	Expect(err).NotTo(HaveOccurred())

	// Register reconcilers (no webhooks — envtest doesn't run the webhook server by default).
	recorder := mgr.GetEventRecorderFor("redis-operator-e2e")
	clusterReconciler := cluster.NewClusterReconciler(mgr.GetClient(), mgr.GetScheme(), recorder)
	Expect(clusterReconciler.SetupWithManager(mgr)).To(Succeed())

	backupReconciler := backup.NewBackupReconciler(mgr.GetClient(), mgr.GetScheme(), recorder)
	Expect(backupReconciler.SetupWithManager(mgr)).To(Succeed())

	scheduledBackupReconciler := backup.NewScheduledBackupReconciler(mgr.GetClient(), mgr.GetScheme(), recorder)
	Expect(scheduledBackupReconciler.SetupWithManager(mgr)).To(Succeed())

	// Webhooks require a serving certificate; skip in envtest unless explicitly configured.
	// To enable webhook testing, set ENABLE_WEBHOOKS=true and configure envtest webhook options.
	if os.Getenv("ENABLE_WEBHOOKS") == "true" {
		defaulter := &webhooks.RedisClusterDefaulter{}
		Expect(defaulter.SetupWebhookWithManager(mgr)).To(Succeed())

		validator := &webhooks.RedisClusterValidator{}
		Expect(validator.SetupValidatingWebhookWithManager(mgr)).To(Succeed())
	}

	var ctx context.Context
	ctx, cancelFunc = context.WithCancel(context.Background())

	// Start manager in background.
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	if cancelFunc != nil {
		cancelFunc()
	}
	// Only stop testEnv if Start() succeeded (cfg is set). testEnv is assigned
	// before Start() is called, so a nil check on testEnv is not sufficient —
	// calling Stop() on an unstarted Environment panics.
	if cfg != nil {
		Expect(testEnv.Stop()).To(Succeed())
	}
})

// isEnvtestBinaryMissing returns true if the error looks like envtest binaries
// are not installed, enabling graceful skipping in CI environments.
func isEnvtestBinaryMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such file") ||
		strings.Contains(msg, "binary") ||
		strings.Contains(msg, "kube-apiserver") ||
		strings.Contains(msg, "etcd") ||
		strings.Contains(msg, "KUBEBUILDER_ASSETS")
}

// uniqueName generates a unique test resource name using a random suffix.
func uniqueName(prefix string) string {
	return prefix + "-" + rand.String(6)
}
