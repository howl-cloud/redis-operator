package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

var _ = Describe("Hibernation", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	Describe("Hibernate and resume", func() {
		It("hibernates a cluster: deletes pods while retaining PVCs", func() {
			name := uniqueName("hibernate")
			spec := helpers.MakeBasicClusterSpec(2)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				// Clean up even if still hibernated.
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Wait for the reconciler to create pods.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 2, reconcileTimeout)).To(Succeed())

			// Wait for PVCs to be created.
			Eventually(func(g Gomega) {
				var pvcList corev1.PersistentVolumeClaimList
				g.Expect(k8sClient.List(ctx, &pvcList,
					client.InNamespace(namespace),
					client.MatchingLabels{redisv1.LabelCluster: name},
				)).To(Succeed())
				g.Expect(pvcList.Items).To(HaveLen(2))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Annotate for hibernation.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())

				patch := client.MergeFrom(fresh.DeepCopy())
				if fresh.Annotations == nil {
					fresh.Annotations = make(map[string]string)
				}
				fresh.Annotations[redisv1.AnnotationHibernation] = "on"
				g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Wait for phase=Hibernating.
			Expect(helpers.WaitForPhase(ctx, k8sClient, cluster, redisv1.ClusterPhaseHibernating, reconcileTimeout)).To(Succeed())

			// Assert all pods are deleted.
			Eventually(func(g Gomega) {
				var podList corev1.PodList
				g.Expect(k8sClient.List(ctx, &podList,
					client.InNamespace(namespace),
					client.MatchingLabels{redisv1.LabelCluster: name},
				)).To(Succeed())
				g.Expect(podList.Items).To(BeEmpty(), "all pods should be deleted during hibernation")
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Assert PVCs are retained.
			var pvcList corev1.PersistentVolumeClaimList
			Expect(k8sClient.List(ctx, &pvcList,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)).To(Succeed())
			Expect(pvcList.Items).To(HaveLen(2), "PVCs must be retained during hibernation")

			// Assert the Hibernated condition is True.
			Expect(helpers.WaitForCondition(ctx, k8sClient, cluster, redisv1.ConditionHibernated, reconcileTimeout)).To(Succeed())
		})

		It("resumes from hibernation: recreates pods and transitions to Creating phase", func() {
			name := uniqueName("resume")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Wait for initial pod.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 1, reconcileTimeout)).To(Succeed())

			// Hibernate.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())

				patch := client.MergeFrom(fresh.DeepCopy())
				if fresh.Annotations == nil {
					fresh.Annotations = make(map[string]string)
				}
				fresh.Annotations[redisv1.AnnotationHibernation] = "on"
				g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Wait for Hibernating phase.
			Expect(helpers.WaitForPhase(ctx, k8sClient, cluster, redisv1.ClusterPhaseHibernating, reconcileTimeout)).To(Succeed())

			// Confirm no pods.
			Eventually(func(g Gomega) {
				var podList corev1.PodList
				g.Expect(k8sClient.List(ctx, &podList,
					client.InNamespace(namespace),
					client.MatchingLabels{redisv1.LabelCluster: name},
				)).To(Succeed())
				g.Expect(podList.Items).To(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Remove the hibernation annotation to resume.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())

				patch := client.MergeFrom(fresh.DeepCopy())
				delete(fresh.Annotations, redisv1.AnnotationHibernation)
				g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// The reconciler should resume: first set phase=Creating, then recreate pods.
			// We assert on phase transition away from Hibernating.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(fresh.Status.Phase).NotTo(Equal(redisv1.ClusterPhaseHibernating),
					"phase should transition away from Hibernating after resume")
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Pods should be recreated.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 1, reconcileTimeout)).To(Succeed())
		})

		It("preserves the Hibernated=False condition after resuming", func() {
			name := uniqueName("cond-resume")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Quick hibernate/resume cycle.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				patch := client.MergeFrom(fresh.DeepCopy())
				if fresh.Annotations == nil {
					fresh.Annotations = make(map[string]string)
				}
				fresh.Annotations[redisv1.AnnotationHibernation] = "on"
				g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			Expect(helpers.WaitForPhase(ctx, k8sClient, cluster, redisv1.ClusterPhaseHibernating, reconcileTimeout)).To(Succeed())

			// Remove annotation.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				patch := client.MergeFrom(fresh.DeepCopy())
				delete(fresh.Annotations, redisv1.AnnotationHibernation)
				g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// The Hibernated condition should become False after resume.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())

				var found bool
				for _, cond := range fresh.Status.Conditions {
					if cond.Type == redisv1.ConditionHibernated {
						found = true
						g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
						g.Expect(cond.Reason).To(Equal("HibernationDisabled"))
					}
				}
				g.Expect(found).To(BeTrue(), "Hibernated condition should be present after resume")
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})
	})
})
