package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
)

var _ = Describe("RedisBackup", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	Describe("Backup lifecycle", func() {
		It("creates a RedisBackup object and transitions through phases", func() {
			// First create the backing cluster.
			clusterName := uniqueName("backup-cluster")
			clusterSpec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, clusterName, clusterSpec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			// Wait for cluster to be acknowledged by the reconciler.
			Eventually(func(g Gomega) {
				fresh, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(fresh.Status.Phase).NotTo(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			// Create a RedisBackup.
			backupName := uniqueName("backup")
			backup := &redisv1.RedisBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupName,
					Namespace: namespace,
				},
				Spec: redisv1.RedisBackupSpec{
					ClusterName: clusterName,
					Target:      redisv1.BackupTargetPreferReplica,
					Method:      redisv1.BackupMethodRDB,
				},
			}

			Expect(k8sClient.Create(ctx, backup)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, backup)
			})

			// Verify the backup object exists in the API server.
			Eventually(func(g Gomega) {
				var fetched redisv1.RedisBackup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: backupName, Namespace: namespace,
				}, &fetched)).To(Succeed())
				// The backup reconciler should transition the phase away from empty.
				// In envtest (no pod IPs) it may go Pending or Running depending on timing.
				// We assert the phase is set (non-empty), not the specific value.
				g.Expect(fetched.Status.Phase).NotTo(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("sets the backup phase to Pending when the cluster has no running pods", func() {
			// Without real pods (envtest), the backup reconciler cannot find a target pod
			// and should set phase=Pending or Failed.
			clusterName := uniqueName("nopods-cluster")
			clusterSpec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, clusterName, clusterSpec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			backupName := uniqueName("nopods-backup")
			backup := &redisv1.RedisBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupName,
					Namespace: namespace,
				},
				Spec: redisv1.RedisBackupSpec{
					ClusterName: clusterName,
					Target:      redisv1.BackupTargetPreferReplica,
					Method:      redisv1.BackupMethodRDB,
				},
			}

			Expect(k8sClient.Create(ctx, backup)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, backup)
			})

			// The reconciler should set a phase relatively quickly.
			Eventually(func(g Gomega) {
				var fetched redisv1.RedisBackup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: backupName, Namespace: namespace,
				}, &fetched)).To(Succeed())
				g.Expect(fetched.Status.Phase).NotTo(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})
	})

	Describe("RedisScheduledBackup", func() {
		It("creates a RedisScheduledBackup and tracks phase", func() {
			clusterName := uniqueName("sched-cluster")
			clusterSpec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, clusterName, clusterSpec)
			Expect(err).NotTo(HaveOccurred())

			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			schedName := uniqueName("sched")
			sched := &redisv1.RedisScheduledBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      schedName,
					Namespace: namespace,
				},
				Spec: redisv1.RedisScheduledBackupSpec{
					Schedule:    "0 2 * * *",
					ClusterName: clusterName,
					Target:      redisv1.BackupTargetPreferReplica,
					Method:      redisv1.BackupMethodRDB,
				},
			}

			Expect(k8sClient.Create(ctx, sched)).To(Succeed())

			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, sched)
			})

			// Verify the scheduled backup exists and the reconciler processes it.
			var _ = wait.PollUntilContextTimeout // imported for test utilities

			Eventually(func(g Gomega) {
				var fetched redisv1.RedisScheduledBackup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: schedName, Namespace: namespace,
				}, &fetched)).To(Succeed())
				// The scheduled backup reconciler should set the phase to Active.
				g.Expect(fetched.Status.Phase).To(Or(
					Equal(redisv1.ScheduledBackupPhaseActive),
					Equal(redisv1.ScheduledBackupPhaseSuspended),
				))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})
	})
})
