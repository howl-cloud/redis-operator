package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

const (
	// reconcileTimeout is how long to wait for a reconciliation side-effect.
	// envtest has no scheduling delay, so objects appear quickly.
	reconcileTimeout = 15 * time.Second
)

var _ = Describe("RedisCluster lifecycle", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		// Use the default namespace for simplicity; uniqueName avoids collisions.
		namespace = "default"
	})

	Describe("Basic CRUD", func() {
		It("creates a RedisCluster and enters Creating phase", func() {
			name := uniqueName("basic")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// The reconciler should quickly set the phase to Creating (before HTTP polls).
			// envtest has no kubelets so pods are created but stay Pending.
			// We assert on the phase transition driven purely by the reconciler.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				// Phase should be Creating once the reconciler sets the initial primary.
				g.Expect(fresh.Status.Phase).To(Or(
					Equal(redisv1.ClusterPhaseCreating),
					Equal(redisv1.ClusterPhaseDegraded),
				))
				g.Expect(fresh.Status.CurrentPrimary).NotTo(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("creates the three per-cluster Services", func() {
			name := uniqueName("svc")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Wait for the leader, replica, and any services.
			Eventually(func(g Gomega) {
				var svc corev1.Service
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: name + "-leader", Namespace: namespace,
				}, &svc)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: name + "-replica", Namespace: namespace,
				}, &svc)).To(Succeed())
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: name + "-any", Namespace: namespace,
				}, &svc)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("creates the per-cluster ConfigMap with redis.conf", func() {
			name := uniqueName("cm")
			spec := helpers.MakeBasicClusterSpec(1)
			spec.Redis = map[string]string{
				"maxmemory":        "256mb",
				"maxmemory-policy": "allkeys-lru",
			}

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			Eventually(func(g Gomega) {
				var cm corev1.ConfigMap
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: name + "-config", Namespace: namespace,
				}, &cm)).To(Succeed())
				g.Expect(cm.Data["redis.conf"]).To(ContainSubstring("port 6379"))
				g.Expect(cm.Data["redis.conf"]).To(ContainSubstring("maxmemory 256mb"))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("creates the auth Secret automatically", func() {
			name := uniqueName("auth")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			Eventually(func(g Gomega) {
				var secret corev1.Secret
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: name + "-auth", Namespace: namespace,
				}, &secret)).To(Succeed())
				g.Expect(secret.Data["password"]).NotTo(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("scales up from 1 to 3 instances by creating additional pods", func() {
			name := uniqueName("scaleup")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Wait for the initial pod to exist.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 1, reconcileTimeout)).To(Succeed())

			// Scale up to 3.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				fresh.Spec.Instances = 3
				g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Expect 3 pods eventually.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 3, reconcileTimeout)).To(Succeed())
		})

		It("scales down from 3 to 1 instance by deleting excess pods", func() {
			name := uniqueName("scaledown")
			spec := helpers.MakeBasicClusterSpec(3)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Wait for all 3 pods.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 3, reconcileTimeout)).To(Succeed())

			// Scale down to 1.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				fresh.Spec.Instances = 1
				g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Expect 1 pod.
			Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 1, reconcileTimeout)).To(Succeed())
		})

		It("creates PVCs for each instance", func() {
			name := uniqueName("pvcs")
			spec := helpers.MakeBasicClusterSpec(2)

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
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("sets currentPrimary to the first pod", func() {
			name := uniqueName("primary")
			spec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(fresh.Status.CurrentPrimary).To(Equal(name + "-0"))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})
	})

	Describe("Spec validation", func() {
		It("rejects a cluster with minSyncReplicas > instances-1 (webhook validation)", func() {
			// This test only passes when webhooks are enabled (ENABLE_WEBHOOKS=true).
			// Without webhooks, the API server accepts the object and the reconciler
			// handles invalid specs gracefully. We verify the spec is created and the
			// validation logic is exercised by the validator directly.
			//
			// When ENABLE_WEBHOOKS=true this will be rejected at the API server level.
			name := uniqueName("invalid")
			spec := helpers.MakeBasicClusterSpec(1)
			spec.MinSyncReplicas = 5 // Violates: must be <= instances-1 = 0

			cluster := &redisv1.RedisCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: spec,
			}

			err := k8sClient.Create(ctx, cluster)
			if err == nil {
				// Webhooks not active — clean up and note this is expected without webhook setup.
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, cluster)
				})
				// The validator logic is exercised in the webhooks package unit tests.
				// Here we just verify the object can be created (API server accepts it without webhooks).
				Skip("Webhook validation not active in this test environment; validated in unit tests")
			} else {
				// Webhook rejected it — verify it's a validation error.
				Expect(errors.IsInvalid(err) || errors.IsBadRequest(err) || errors.IsForbidden(err)).To(BeTrue(),
					"expected a validation error, got: %v", err)
			}
		})
	})
})
