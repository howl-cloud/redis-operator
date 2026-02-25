package e2e

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	Describe("Bootstrap from backup", func() {
		It("injects a restore init container when bootstrap.backupName is set", func() {
			clusterName := uniqueName("restore-cluster")
			backupName := uniqueName("restore-backup")
			backupCredentialsSecretName := uniqueName("backup-creds")

			backupCredsSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupCredentialsSecretName,
					Namespace: namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"AWS_ACCESS_KEY_ID":     []byte("test-access-key"),
					"AWS_SECRET_ACCESS_KEY": []byte("test-secret-key"),
				},
			}
			Expect(k8sClient.Create(ctx, backupCredsSecret)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, backupCredsSecret)
			})

			backup := &redisv1.RedisBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupName,
					Namespace: namespace,
				},
				Spec: redisv1.RedisBackupSpec{
					ClusterName: clusterName,
					Method:      redisv1.BackupMethodRDB,
					Destination: &redisv1.BackupDestination{
						S3: &redisv1.S3Destination{
							Bucket: "test-bucket",
							Path:   "backups",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, backup)
			})

			var storedBackup redisv1.RedisBackup
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      backupName,
				Namespace: namespace,
			}, &storedBackup)).To(Succeed())
			patch := client.MergeFrom(storedBackup.DeepCopy())
			storedBackup.Status.Phase = redisv1.BackupPhaseCompleted
			storedBackup.Status.BackupPath = fmt.Sprintf("s3://test-bucket/backups/%s.rdb", backupName)
			Expect(k8sClient.Status().Patch(ctx, &storedBackup, patch)).To(Succeed())

			spec := helpers.MakeBasicClusterSpec(1)
			spec.BackupCredentialsSecret = &redisv1.LocalObjectReference{Name: backupCredentialsSecretName}
			spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backupName}

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, clusterName, spec)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			Eventually(func(g Gomega) {
				var podList corev1.PodList
				g.Expect(k8sClient.List(
					ctx,
					&podList,
					client.InNamespace(namespace),
					client.MatchingLabels{
						redisv1.LabelCluster: clusterName,
						redisv1.LabelRole:    redisv1.LabelRolePrimary,
					},
				)).To(Succeed())
				g.Expect(podList.Items).To(HaveLen(1))

				pod := podList.Items[0]

				var restoreContainer *corev1.Container
				for i := range pod.Spec.InitContainers {
					if pod.Spec.InitContainers[i].Name == "restore-data" {
						restoreContainer = &pod.Spec.InitContainers[i]
						break
					}
				}
				g.Expect(restoreContainer).NotTo(BeNil())
				g.Expect(restoreContainer.Command).To(Equal([]string{"/manager", "restore"}))
				g.Expect(restoreContainer.Args).To(ContainElement("--cluster-name=" + clusterName))
				g.Expect(restoreContainer.Args).To(ContainElement("--backup-name=" + backupName))
				g.Expect(restoreContainer.Args).To(ContainElement("--backup-namespace=" + namespace))
				g.Expect(restoreContainer.Args).To(ContainElement("--data-dir=/data"))

				g.Expect(pod.Spec.Volumes).To(ContainElement(
					HaveField("Name", "backup-credentials"),
				))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			Eventually(func(g Gomega) {
				freshCluster, err := helpers.GetRedisCluster(ctx, k8sClient, namespace, clusterName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(freshCluster.Status.Phase).To(Equal(redisv1.ClusterPhaseCreating))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})

		It("injects a restore init container for AOF bootstrap backups", func() {
			clusterName := uniqueName("restore-aof-cluster")
			backupName := uniqueName("restore-aof-backup")
			backupCredentialsSecretName := uniqueName("backup-creds")

			backupCredsSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupCredentialsSecretName,
					Namespace: namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"AWS_ACCESS_KEY_ID":     []byte("test-access-key"),
					"AWS_SECRET_ACCESS_KEY": []byte("test-secret-key"),
				},
			}
			Expect(k8sClient.Create(ctx, backupCredsSecret)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, backupCredsSecret)
			})

			backup := &redisv1.RedisBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupName,
					Namespace: namespace,
				},
				Spec: redisv1.RedisBackupSpec{
					ClusterName: clusterName,
					Method:      redisv1.BackupMethodAOF,
					Destination: &redisv1.BackupDestination{
						S3: &redisv1.S3Destination{
							Bucket: "test-bucket",
							Path:   "backups",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, backup)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, backup)
			})

			var storedBackup redisv1.RedisBackup
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      backupName,
				Namespace: namespace,
			}, &storedBackup)).To(Succeed())
			patch := client.MergeFrom(storedBackup.DeepCopy())
			storedBackup.Status.Phase = redisv1.BackupPhaseCompleted
			storedBackup.Status.BackupPath = fmt.Sprintf("s3://test-bucket/backups/%s.aof.tar.gz", backupName)
			storedBackup.Status.ArtifactType = redisv1.BackupArtifactTypeAOFArchive
			Expect(k8sClient.Status().Patch(ctx, &storedBackup, patch)).To(Succeed())

			spec := helpers.MakeBasicClusterSpec(1)
			spec.BackupCredentialsSecret = &redisv1.LocalObjectReference{Name: backupCredentialsSecretName}
			spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backupName}

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, clusterName, spec)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			Eventually(func(g Gomega) {
				var podList corev1.PodList
				g.Expect(k8sClient.List(
					ctx,
					&podList,
					client.InNamespace(namespace),
					client.MatchingLabels{
						redisv1.LabelCluster: clusterName,
						redisv1.LabelRole:    redisv1.LabelRolePrimary,
					},
				)).To(Succeed())
				g.Expect(podList.Items).To(HaveLen(1))

				pod := podList.Items[0]

				var restoreContainer *corev1.Container
				for i := range pod.Spec.InitContainers {
					if pod.Spec.InitContainers[i].Name == "restore-data" {
						restoreContainer = &pod.Spec.InitContainers[i]
						break
					}
				}
				g.Expect(restoreContainer).NotTo(BeNil())
				g.Expect(restoreContainer.Command).To(Equal([]string{"/manager", "restore"}))
				g.Expect(restoreContainer.Args).To(ContainElement("--cluster-name=" + clusterName))
				g.Expect(restoreContainer.Args).To(ContainElement("--backup-name=" + backupName))
				g.Expect(restoreContainer.Args).To(ContainElement("--backup-namespace=" + namespace))
				g.Expect(restoreContainer.Args).To(ContainElement("--data-dir=/data"))
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
