package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

func boolPtr(v bool) *bool {
	return &v
}

const maintenanceReconcileTimeout = 45 * time.Second

func setMaintenanceWindow(ctx context.Context, namespace, name string, inProgress bool, reusePVC *bool) {
	Eventually(func(g Gomega) {
		fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
		g.Expect(err).NotTo(HaveOccurred())

		patch := client.MergeFrom(fresh.DeepCopy())
		if fresh.Spec.NodeMaintenanceWindow == nil {
			fresh.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{}
		}
		fresh.Spec.NodeMaintenanceWindow.InProgress = inProgress
		fresh.Spec.NodeMaintenanceWindow.ReusePVC = reusePVC
		g.Expect(k8sClient.Patch(ctx, fresh, patch)).To(Succeed())
	}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
}

func waitForMaintenanceCondition(ctx context.Context, namespace, name string, status metav1.ConditionStatus) {
	Eventually(func(g Gomega) {
		fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
		g.Expect(err).NotTo(HaveOccurred())

		matched := false
		for _, condition := range fresh.Status.Conditions {
			if condition.Type == redisv1.ConditionMaintenanceInProgress {
				matched = true
				g.Expect(condition.Status).To(Equal(status))
				break
			}
		}
		g.Expect(matched).To(BeTrue())
	}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
}

var _ = Describe("Node maintenance window", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	It("suspends pod recreation during maintenance and recovers after maintenance ends", func() {
		name := uniqueName("maintenance")
		spec := helpers.MakeBasicClusterSpec(3)

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 3, reconcileTimeout)).To(Succeed())

		Eventually(func(g Gomega) {
			var pdb policyv1.PodDisruptionBudget
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      name + "-pdb",
				Namespace: namespace,
			}, &pdb)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		setMaintenanceWindow(ctx, namespace, name, true, boolPtr(true))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionTrue)

		Eventually(func() bool {
			var pdb policyv1.PodDisruptionBudget
			err := k8sClient.Get(ctx, types.NamespacedName{
				Name:      name + "-pdb",
				Namespace: namespace,
			}, &pdb)
			return apierrors.IsNotFound(err)
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(BeTrue())

		var podList corev1.PodList
		Expect(k8sClient.List(ctx, &podList,
			client.InNamespace(namespace),
			client.MatchingLabels{redisv1.LabelCluster: name},
		)).To(Succeed())
		Expect(podList.Items).NotTo(BeEmpty())

		drainedPod := podList.Items[0]
		Expect(k8sClient.Delete(ctx, &drainedPod)).To(Succeed())

		Eventually(func() int {
			var pods corev1.PodList
			_ = k8sClient.List(ctx, &pods,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)
			return len(pods.Items)
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Equal(2))

		Consistently(func() int {
			var pods corev1.PodList
			_ = k8sClient.List(ctx, &pods,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)
			return len(pods.Items)
		}, 2*time.Second, helpers.DefaultPollingInterval).Should(Equal(2))

		setMaintenanceWindow(ctx, namespace, name, false, boolPtr(true))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionFalse)

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 3, reconcileTimeout)).To(Succeed())
	})

	It("preserves PVCs when reusePVC is true", func() {
		name := uniqueName("maintenance-pvc")
		spec := helpers.MakeBasicClusterSpec(2)

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 2, reconcileTimeout)).To(Succeed())

		Eventually(func(g Gomega) {
			var pvcList corev1.PersistentVolumeClaimList
			g.Expect(k8sClient.List(ctx, &pvcList,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)).To(Succeed())
			g.Expect(pvcList.Items).To(HaveLen(2))
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		setMaintenanceWindow(ctx, namespace, name, true, boolPtr(true))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionTrue)

		var pod corev1.Pod
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      name + "-1",
				Namespace: namespace,
			}, &pod)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		var claimName string
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				claimName = volume.PersistentVolumeClaim.ClaimName
				break
			}
		}
		Expect(claimName).NotTo(BeEmpty())

		Expect(k8sClient.Delete(ctx, &pod)).To(Succeed())

		Eventually(func() int {
			var pods corev1.PodList
			_ = k8sClient.List(ctx, &pods,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)
			return len(pods.Items)
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Equal(1))

		Eventually(func(g Gomega) {
			var pvc corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      claimName,
				Namespace: namespace,
			}, &pvc)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		setMaintenanceWindow(ctx, namespace, name, false, boolPtr(true))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionFalse)

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 2, reconcileTimeout)).To(Succeed())

		Eventually(func(g Gomega) {
			var pods corev1.PodList
			g.Expect(k8sClient.List(ctx, &pods,
				client.InNamespace(namespace),
				client.MatchingLabels{redisv1.LabelCluster: name},
			)).To(Succeed())

			inUse := false
			for _, restoredPod := range pods.Items {
				for _, volume := range restoredPod.Spec.Volumes {
					if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == claimName {
						inUse = true
						break
					}
				}
				if inUse {
					break
				}
			}
			g.Expect(inUse).To(BeTrue())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})

	It("surfaces MaintenanceInProgress condition transitions", func() {
		name := uniqueName("maintenance-cond")
		spec := helpers.MakeBasicClusterSpec(1)

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 1, reconcileTimeout)).To(Succeed())

		setMaintenanceWindow(ctx, namespace, name, true, boolPtr(true))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionTrue)

		setMaintenanceWindow(ctx, namespace, name, false, boolPtr(true))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionFalse)
	})

	It("replaces missing pod with fresh PVC during maintenance when reusePVC is false", func() {
		name := uniqueName("maintenance-replace")
		spec := helpers.MakeBasicClusterSpec(2)

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 2, reconcileTimeout)).To(Succeed())

		setMaintenanceWindow(ctx, namespace, name, true, boolPtr(false))
		waitForMaintenanceCondition(ctx, namespace, name, metav1.ConditionTrue)

		pvcName := fmt.Sprintf("%s-data-1", name)
		Eventually(func(g Gomega) {
			var pvc corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pvcName,
				Namespace: namespace,
			}, &pvc)).To(Succeed())
			patch := client.MergeFrom(pvc.DeepCopy())
			if pvc.Annotations == nil {
				pvc.Annotations = make(map[string]string)
			}
			pvc.Annotations["test/marker"] = "old"
			g.Expect(k8sClient.Patch(ctx, &pvc, patch)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		var drainedPod corev1.Pod
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      name + "-1",
				Namespace: namespace,
			}, &drainedPod)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		Expect(k8sClient.Delete(ctx, &drainedPod)).To(Succeed())

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 2, maintenanceReconcileTimeout)).To(Succeed())

		Eventually(func(g Gomega) {
			var pvc corev1.PersistentVolumeClaim
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      pvcName,
				Namespace: namespace,
			}, &pvc)).To(Succeed())
			// In envtest, pvc-protection finalizers can leave recycled PVCs in terminating state
			// because no kube-controller-manager removes finalizers. Accept that as evidence the
			// recycle step happened; otherwise require a fresh PVC without the marker annotation.
			if pvc.DeletionTimestamp != nil {
				return
			}
			g.Expect(pvc.Annotations["test/marker"]).To(BeEmpty(),
				"replacement PVC should be newly created and not retain old marker annotation")
		}, maintenanceReconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})
})
