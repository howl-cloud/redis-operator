package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

var _ = Describe("PVC resize", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	It("updates PVC requested storage when cluster storage size increases", func() {
		name := uniqueName("resize")
		spec := helpers.MakeBasicClusterSpec(2)
		spec.Storage.Size = resource.MustParse("1Gi")

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Eventually(func(g Gomega) {
			var pvcList corev1.PersistentVolumeClaimList
			g.Expect(k8sClient.List(ctx, &pvcList,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)).To(Succeed())
			g.Expect(pvcList.Items).To(HaveLen(2))
			for _, pvc := range pvcList.Items {
				requestedStorage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				g.Expect(requestedStorage.Cmp(resource.MustParse("1Gi"))).To(Equal(0))
			}
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		Eventually(func(g Gomega) {
			fresh, getErr := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(getErr).NotTo(HaveOccurred())
			fresh.Spec.Storage.Size = resource.MustParse("2Gi")
			g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		Eventually(func(g Gomega) {
			var pvcList corev1.PersistentVolumeClaimList
			g.Expect(k8sClient.List(ctx, &pvcList,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)).To(Succeed())
			g.Expect(pvcList.Items).To(HaveLen(2))
			for _, pvc := range pvcList.Items {
				requestedStorage := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
				g.Expect(requestedStorage.Cmp(resource.MustParse("2Gi"))).To(Equal(0))
				if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
					g.Expect(capacity.Cmp(resource.MustParse("2Gi"))).To(Equal(0))
				}
			}
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})
})
