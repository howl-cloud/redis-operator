package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

const specHashKey = "redis.io/spec-hash"

var _ = Describe("Rolling Update", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	It("pauses in supervised mode after replicas are updated", func() {
		name := uniqueName("supervised-pause")
		spec := helpers.MakeBasicClusterSpec(3)
		spec.PrimaryUpdateStrategy = redisv1.PrimaryUpdateStrategySupervised

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 3, reconcileTimeout)).To(Succeed())

		var primaryPod string
		var originalHashes map[string]string
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(fresh.Status.CurrentPrimary).NotTo(BeEmpty())

			primaryPod = fresh.Status.CurrentPrimary
			hashes, hashErr := podHashes(ctx, namespace, []string{name + "-0", name + "-1", name + "-2"})
			g.Expect(hashErr).NotTo(HaveOccurred())
			g.Expect(hashes[primaryPod]).NotTo(BeEmpty())
			originalHashes = hashes
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		// Trigger rolling update by changing image.
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())

			patch := client.MergeFrom(fresh.DeepCopy())
			fresh.Spec.ImageName = "redis:7.2.1"
			g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		// Verify replicas advance to the new hash, but primary stays on the old hash while waiting.
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(conditionStatus(fresh.Status.Conditions, redisv1.ConditionPrimaryUpdateWaiting)).To(Equal(metav1.ConditionTrue))

			hashes, hashErr := podHashes(ctx, namespace, []string{name + "-0", name + "-1", name + "-2"})
			g.Expect(hashErr).NotTo(HaveOccurred())
			g.Expect(hashes[primaryPod]).To(Equal(originalHashes[primaryPod]))

			for _, podName := range []string{name + "-0", name + "-1", name + "-2"} {
				if podName == primaryPod {
					continue
				}
				g.Expect(hashes[podName]).NotTo(Equal(originalHashes[podName]))
			}
		}, 2*reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})

	It("resumes supervised primary update when approval annotation is set", func() {
		name := uniqueName("supervised-resume")
		spec := helpers.MakeBasicClusterSpec(1)
		spec.PrimaryUpdateStrategy = redisv1.PrimaryUpdateStrategySupervised

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 1, reconcileTimeout)).To(Succeed())

		var primaryPod string
		var originalHash string
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(fresh.Status.CurrentPrimary).NotTo(BeEmpty())

			primaryPod = fresh.Status.CurrentPrimary
			var pod corev1.Pod
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: primaryPod, Namespace: namespace}, &pod)).To(Succeed())
			originalHash = getPodHash(&pod)
			g.Expect(originalHash).NotTo(BeEmpty())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		// Trigger rolling update by changing image.
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			patch := client.MergeFrom(fresh.DeepCopy())
			fresh.Spec.ImageName = "redis:7.2.1"
			g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		// Wait until the operator reports waiting-for-approval.
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(conditionStatus(fresh.Status.Conditions, redisv1.ConditionPrimaryUpdateWaiting)).To(Equal(metav1.ConditionTrue))
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		// Approve primary update.
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())

			patch := client.MergeFrom(fresh.DeepCopy())
			if fresh.Annotations == nil {
				fresh.Annotations = map[string]string{}
			}
			fresh.Annotations[redisv1.AnnotationApprovePrimaryUpdate] = "true"
			g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		// Verify primary replacement proceeds and approval annotation is cleared.
		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			if fresh.Annotations != nil {
				g.Expect(fresh.Annotations).NotTo(HaveKey(redisv1.AnnotationApprovePrimaryUpdate))
			}
			g.Expect(conditionStatus(fresh.Status.Conditions, redisv1.ConditionPrimaryUpdateWaiting)).To(Equal(metav1.ConditionFalse))

			var pod corev1.Pod
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: primaryPod, Namespace: namespace}, &pod)).To(Succeed())
			g.Expect(getPodHash(&pod)).NotTo(Equal(originalHash))
		}, 2*reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})
})

func podHashes(ctx context.Context, namespace string, podNames []string) (map[string]string, error) {
	hashes := make(map[string]string, len(podNames))
	for _, podName := range podNames {
		var pod corev1.Pod
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
			return nil, err
		}
		hashes[podName] = getPodHash(&pod)
	}
	return hashes, nil
}

func getPodHash(pod *corev1.Pod) string {
	if pod.Annotations != nil {
		if hash, ok := pod.Annotations[specHashKey]; ok {
			return hash
		}
	}
	if pod.Labels != nil {
		if hash, ok := pod.Labels[specHashKey]; ok {
			return hash
		}
	}
	return ""
}

func conditionStatus(conditions []metav1.Condition, conditionType string) metav1.ConditionStatus {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return conditions[i].Status
		}
	}
	return metav1.ConditionUnknown
}
