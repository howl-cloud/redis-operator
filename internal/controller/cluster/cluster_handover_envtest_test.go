package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestClusterHandoverAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run handover API tests")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cluster handover API suite")
}

var _ = Describe("Planned cluster handover", func() {
	It("persists fencing before promotion, survives retries, and preserves emergency fences", func() {
		ctx := context.Background()
		env := &envtest.Environment{CRDDirectoryPaths: []string{"../../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
		cfg, err := env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })
		scheme := testScheme()
		k8s, err := client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())
		cluster := newClusterModeCluster(3, 0)
		cluster.Annotations = map[string]string{redisv1.FencingAnnotationKey: `["test-9"]`}
		Expect(k8s.Create(ctx, cluster)).To(Succeed())
		r := NewClusterReconciler(k8s, scheme, record.NewFakeRecorder(100), 0)
		source := ownerStatus("n3", calculateClusterSlotRanges(3)[0])
		target := replicaStatus("n0", "n3")
		target.MasterLinkStatus = "up"
		statuses := map[string]redisv1.InstanceStatus{"test-3": source, "test-0": target}
		pods := []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "test-3"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "test-0"}, Status: corev1.PodStatus{PodIP: "127.0.0.1"}},
		}
		// A rejected promotion must leave the durable fence available for retry.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			observed := &redisv1.RedisCluster{}
			if err := k8s.Get(req.Context(), client.ObjectKeyFromObject(cluster), observed); err != nil || observed.Annotations[redisv1.ClusterHandoverAnnotation] != "test-3/test-0" || !strings.Contains(observed.Annotations[redisv1.FencingAnnotationKey], "test-3") {
				http.Error(w, "promotion arrived without its fence", http.StatusBadRequest)
				return
			}
			http.Error(w, "retry promotion", http.StatusServiceUnavailable)
		}))
		DeferCleanup(server.Close)
		previous := instanceManagerPort
		instanceManagerPort, err = strconv.Atoi(strings.TrimPrefix(server.URL, "http://127.0.0.1:"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { instanceManagerPort = previous })
		Expect(r.beginClusterHandover(ctx, cluster, "test-3", "test-0")).To(Succeed())
		ready, err := r.resumeClusterHandover(ctx, cluster, pods, statuses)
		Expect(err).NotTo(HaveOccurred())
		Expect(ready).To(BeFalse())
		pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
		_, err = r.resumeClusterHandover(ctx, cluster, pods, statuses)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("status 503"))
		Expect(k8s.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		Expect(r.getFencedPods(cluster)).To(ConsistOf("test-3", "test-9"))

		// Recreate the reconciler as after a manager restart and observe both roles.
		r = NewClusterReconciler(k8s, scheme, record.NewFakeRecorder(100), 0)
		statuses["test-0"] = ownerStatus("n0", source.SlotsServed[0])
		_, err = r.resumeClusterHandover(ctx, cluster, pods, statuses)
		Expect(err).NotTo(HaveOccurred())
		Expect(cluster.Annotations[redisv1.ClusterHandoverAnnotation]).NotTo(BeEmpty())
		statuses["test-3"] = replicaStatus("n3", "n0")
		_, err = r.resumeClusterHandover(ctx, cluster, pods, statuses)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		Expect(cluster.Annotations[redisv1.ClusterHandoverAnnotation]).To(BeEmpty())
		Expect(r.getFencedPods(cluster)).To(ConsistOf("test-9"))

		Expect(r.beginClusterHandover(ctx, cluster, "test-3", "test-0")).To(Succeed())
		Expect(r.setFence(ctx, cluster, "test-3")).To(Succeed())
		Expect(k8s.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		Expect(cluster.Annotations[redisv1.ClusterHandoverAnnotation]).To(BeEmpty())
		Expect(r.getFencedPods(cluster)).To(ConsistOf("test-3", "test-9"))
		Expect(r.beginClusterHandover(ctx, cluster, "test-3", "test-0")).NotTo(Succeed())
		statuses["test-3"] = source
		statuses["test-0"] = target
		Expect(r.clearFence(ctx, cluster, "test-3")).To(Succeed())
		Expect(r.beginClusterHandover(ctx, cluster, "test-3", "test-0")).To(Succeed())
		Expect(r.setFence(ctx, cluster, "test-0")).To(Succeed())
		_, err = r.resumeClusterHandover(ctx, cluster, pods, statuses)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8s.Get(ctx, client.ObjectKeyFromObject(cluster), cluster)).To(Succeed())
		Expect(cluster.Annotations[redisv1.ClusterHandoverAnnotation]).To(BeEmpty())
		Expect(r.getFencedPods(cluster)).To(ConsistOf("test-0", "test-9"))
		Expect(r.beginClusterHandover(ctx, cluster, "test-3", "test-0")).NotTo(Succeed())

	})
})
