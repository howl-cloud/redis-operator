package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

var _ = Describe("Ephemeral storage", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	// dataVolumeIsEmptyDir asserts every data pod backs /data with an emptyDir.
	dataVolumeIsEmptyDir := func(g Gomega, name string) {
		var podList corev1.PodList
		g.Expect(k8sClient.List(ctx, &podList,
			client.InNamespace(namespace),
			client.MatchingLabels{
				redisv1.LabelCluster:  name,
				redisv1.LabelWorkload: redisv1.LabelWorkloadData,
			},
		)).To(Succeed())
		g.Expect(podList.Items).NotTo(BeEmpty())
		for _, pod := range podList.Items {
			var dataVol *corev1.Volume
			for i := range pod.Spec.Volumes {
				if pod.Spec.Volumes[i].Name == "data" {
					dataVol = &pod.Spec.Volumes[i]
					break
				}
			}
			g.Expect(dataVol).NotTo(BeNil())
			g.Expect(dataVol.EmptyDir).NotTo(BeNil(), "data volume must be emptyDir")
			g.Expect(dataVol.PersistentVolumeClaim).To(BeNil(), "data volume must not be PVC-backed")
		}
	}

	noPVCsCreated := func(g Gomega, name string) {
		var pvcList corev1.PersistentVolumeClaimList
		g.Expect(k8sClient.List(ctx, &pvcList,
			client.InNamespace(namespace),
			client.MatchingLabels{redisv1.LabelCluster: name},
		)).To(Succeed())
		g.Expect(pvcList.Items).To(BeEmpty(), "ephemeral clusters must not create PVCs")
	}

	It("reconciles a standalone ephemeral cluster without PVCs", func() {
		name := uniqueName("ephemeral-standalone")
		spec := helpers.MakeBasicClusterSpec(2)
		spec.Storage.Type = redisv1.StorageTypeEmptyDir

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Eventually(func(g Gomega) {
			fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(fresh.Status.CurrentPrimary).NotTo(BeEmpty())
			g.Expect(fresh.Status.HealthyPVC).To(BeEquivalentTo(0))
			g.Expect(fresh.Status.DanglingPVC).To(BeEmpty())

			dataVolumeIsEmptyDir(g, name)
			noPVCsCreated(g, name)
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})

	It("reconciles a sentinel ephemeral cluster without PVCs", func() {
		name := uniqueName("ephemeral-sentinel")
		spec := helpers.MakeBasicClusterSpec(3)
		spec.Mode = redisv1.ClusterModeSentinel
		spec.Storage.Type = redisv1.StorageTypeEmptyDir

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Eventually(func(g Gomega) {
			fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(fresh.Status.HealthyPVC).To(BeEquivalentTo(0))
			g.Expect(fresh.Status.DanglingPVC).To(BeEmpty())

			dataVolumeIsEmptyDir(g, name)
			noPVCsCreated(g, name)
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})
})
