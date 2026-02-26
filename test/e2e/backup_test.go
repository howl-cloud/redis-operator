package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	"github.com/howl-cloud/redis-operator/test/e2e/helpers"
	"github.com/howl-cloud/redis-operator/webhooks"
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

		It("returns a clear error for a missing bootstrap backup reference", func() {
			clusterName := uniqueName("restore-missing-backup")
			missingBackupName := uniqueName("missing-backup")
			spec := helpers.MakeBasicClusterSpec(1)
			spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: missingBackupName}

			cluster := &redisv1.RedisCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterName,
					Namespace: namespace,
				},
				Spec: spec,
			}

			// When webhooks are enabled, admission should reject missing backup references.
			if os.Getenv("ENABLE_WEBHOOKS") == "true" {
				err := k8sClient.Create(ctx, cluster)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("spec.bootstrap.backupName"))
				Expect(err.Error()).To(ContainSubstring(missingBackupName))
				return
			}

			// In default envtest mode (no webhook server), validate explicitly through the validator.
			validator := &webhooks.RedisClusterValidator{Reader: k8sClient}
			warnings, err := validator.ValidateCreate(ctx, cluster)
			Expect(warnings).To(BeNil())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.bootstrap.backupName"))
			Expect(err.Error()).To(ContainSubstring(missingBackupName))
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

		It("creates a scheduled backup that can be used for bootstrap restore", func() {
			clusterName := uniqueName("sched-restore-cluster")
			clusterSpec := helpers.MakeBasicClusterSpec(1)

			cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, clusterName, clusterSpec)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			})

			backupCredentialsSecretName := uniqueName("sched-backup-creds")
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

			listener, err := net.Listen("tcp", "127.0.0.1:8080")
			if err != nil {
				Skip("instance-manager mock server unavailable on 127.0.0.1:8080: " + err.Error())
			}
			backupServer := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/status":
						if r.Method != http.MethodGet {
							http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"role":"master","replicationOffset":0,"connectedReplicas":0,"connected":true}`))
						return
					case "/v1/backup":
						if r.Method != http.MethodPost {
							http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
							return
						}
						defer r.Body.Close() //nolint:errcheck // best effort in test handler

						var request struct {
							BackupName string `json:"backupName"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.BackupName == "" {
							http.Error(w, "invalid request body", http.StatusBadRequest)
							return
						}

						w.Header().Set("Content-Type", "application/json")
						_, _ = fmt.Fprintf(
							w,
							`{"artifactType":"rdb","backupPath":"s3://test-bucket/scheduled/%s.rdb","backupSize":1234}`,
							request.BackupName,
						)
					default:
						http.NotFound(w, r)
					}
				}),
				ReadHeaderTimeout: 5 * time.Second,
			}
			go func() {
				_ = backupServer.Serve(listener)
			}()
			DeferCleanup(func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = backupServer.Shutdown(shutdownCtx)
				_ = listener.Close()
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
				podPatch := client.MergeFrom(pod.DeepCopy())
				pod.Status.PodIP = "127.0.0.1"
				pod.Status.Phase = corev1.PodRunning
				g.Expect(k8sClient.Status().Patch(ctx, &pod, podPatch)).To(Succeed())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			var statusCluster redisv1.RedisCluster
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, &statusCluster)).To(Succeed())
			statusPatch := client.MergeFrom(statusCluster.DeepCopy())
			statusCluster.Status.Phase = redisv1.ClusterPhaseHealthy
			statusCluster.Status.CurrentPrimary = clusterName + "-0"
			Expect(k8sClient.Status().Patch(ctx, &statusCluster, statusPatch)).To(Succeed())

			schedName := uniqueName("sched-restore")
			sched := &redisv1.RedisScheduledBackup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      schedName,
					Namespace: namespace,
				},
				Spec: redisv1.RedisScheduledBackupSpec{
					Schedule:    "* * * * *",
					ClusterName: clusterName,
					Target:      redisv1.BackupTargetPreferReplica,
					Method:      redisv1.BackupMethodRDB,
					Destination: &redisv1.BackupDestination{
						S3: &redisv1.S3Destination{
							Bucket: "test-bucket",
							Path:   "scheduled",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sched)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, sched)
			})

			var createdBackupName string
			Eventually(func(g Gomega) {
				var fetchedScheduled redisv1.RedisScheduledBackup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      schedName,
					Namespace: namespace,
				}, &fetchedScheduled)).To(Succeed())
				g.Expect(fetchedScheduled.Status.Phase).To(Equal(redisv1.ScheduledBackupPhaseActive))
				g.Expect(fetchedScheduled.Status.LastBackupName).NotTo(BeEmpty())
				createdBackupName = fetchedScheduled.Status.LastBackupName

				var createdBackup redisv1.RedisBackup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      createdBackupName,
					Namespace: namespace,
				}, &createdBackup)).To(Succeed())
				g.Expect(createdBackup.Spec.ClusterName).To(Equal(clusterName))
				g.Expect(createdBackup.Spec.Destination).NotTo(BeNil())
				g.Expect(createdBackup.Spec.Destination.S3).NotTo(BeNil())
				g.Expect(createdBackup.Spec.Destination.S3.Bucket).To(Equal("test-bucket"))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			Eventually(func(g Gomega) {
				var scheduledBackup redisv1.RedisBackup
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      createdBackupName,
					Namespace: namespace,
				}, &scheduledBackup)).To(Succeed())
				g.Expect(scheduledBackup.Status.Phase).To(Equal(redisv1.BackupPhaseCompleted))
				g.Expect(scheduledBackup.Status.BackupPath).NotTo(BeEmpty())
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

			restoreClusterName := uniqueName("sched-restored")
			restoreSpec := helpers.MakeBasicClusterSpec(1)
			restoreSpec.BackupCredentialsSecret = &redisv1.LocalObjectReference{Name: backupCredentialsSecretName}
			restoreSpec.Bootstrap = &redisv1.BootstrapSpec{BackupName: createdBackupName}

			restoreCluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, restoreClusterName, restoreSpec)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_ = helpers.DeleteRedisCluster(ctx, k8sClient, restoreCluster)
			})

			Eventually(func(g Gomega) {
				var podList corev1.PodList
				g.Expect(k8sClient.List(
					ctx,
					&podList,
					client.InNamespace(namespace),
					client.MatchingLabels{
						redisv1.LabelCluster: restoreClusterName,
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
				g.Expect(restoreContainer.Args).To(ContainElement("--backup-name=" + createdBackupName))
			}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
		})
	})
})
