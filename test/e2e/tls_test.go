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

var _ = Describe("TLS cluster wiring", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = "default"
	})

	It("projects TLS secrets into /tls and tracks secret rotation without recreating pods", func() {
		name := uniqueName("tls")
		tlsSecretName := name + "-tls"
		caSecretName := name + "-ca"

		tlsSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tlsSecretName,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": []byte("cert-v1"),
				"tls.key": []byte("key-v1"),
			},
		}
		Expect(k8sClient.Create(ctx, tlsSecret)).To(Succeed())

		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      caSecretName,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"ca.crt": []byte("ca-v1"),
			},
		}
		Expect(k8sClient.Create(ctx, caSecret)).To(Succeed())

		spec := helpers.MakeBasicClusterSpec(2)
		spec.TLSSecret = &redisv1.LocalObjectReference{Name: tlsSecretName}
		spec.CASecret = &redisv1.LocalObjectReference{Name: caSecretName}

		cluster, err := helpers.CreateRedisCluster(ctx, k8sClient, namespace, name, spec)
		Expect(err).NotTo(HaveOccurred())

		DeferCleanup(func() {
			_ = helpers.DeleteRedisCluster(ctx, k8sClient, cluster)
			_ = k8sClient.Delete(ctx, tlsSecret)
			_ = k8sClient.Delete(ctx, caSecret)
		})

		Expect(helpers.WaitForPodCount(ctx, k8sClient, cluster, 2, reconcileTimeout)).To(Succeed())

		var pod corev1.Pod
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-0", Namespace: namespace}, &pod)).To(Succeed())

		foundTLSVolume := false
		for _, volume := range pod.Spec.Volumes {
			if volume.Name != "tls-certs" {
				continue
			}
			foundTLSVolume = true
			Expect(volume.Projected).NotTo(BeNil())
			Expect(volume.Projected.Sources).To(HaveLen(2))
			Expect(volume.Projected.Sources[0].Secret).NotTo(BeNil())
			Expect(volume.Projected.Sources[0].Secret.Name).To(Equal(tlsSecretName))
			Expect(volume.Projected.Sources[1].Secret).NotTo(BeNil())
			Expect(volume.Projected.Sources[1].Secret.Name).To(Equal(caSecretName))
		}
		Expect(foundTLSVolume).To(BeTrue(), "expected tls-certs projected volume")

		foundTLSMount := false
		for _, mount := range pod.Spec.Containers[0].VolumeMounts {
			if mount.Name == "tls-certs" && mount.MountPath == "/tls" && mount.ReadOnly {
				foundTLSMount = true
				break
			}
		}
		Expect(foundTLSMount).To(BeTrue(), "expected read-only /tls mount")

		var fresh redisv1.RedisCluster
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &fresh)).To(Succeed())
			g.Expect(fresh.Status.SecretsResourceVersion).To(HaveKey(tlsSecretName))
			g.Expect(fresh.Status.SecretsResourceVersion).To(HaveKey(caSecretName))
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		oldTLSRV := fresh.Status.SecretsResourceVersion[tlsSecretName]
		originalUID := pod.UID

		Eventually(func(g Gomega) {
			var secret corev1.Secret
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tlsSecretName, Namespace: namespace}, &secret)).To(Succeed())
			patch := client.MergeFrom(secret.DeepCopy())
			secret.Data["tls.crt"] = []byte("cert-v2")
			secret.Data["tls.key"] = []byte("key-v2")
			g.Expect(k8sClient.Patch(ctx, &secret, patch)).To(Succeed())
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		Eventually(func(g Gomega) {
			var rotated redisv1.RedisCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &rotated)).To(Succeed())
			g.Expect(rotated.Status.SecretsResourceVersion[tlsSecretName]).NotTo(Equal(oldTLSRV))
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())

		Eventually(func(g Gomega) {
			var currentPod corev1.Pod
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-0", Namespace: namespace}, &currentPod)).To(Succeed())
			g.Expect(currentPod.UID).To(Equal(originalUID))
		}, reconcileTimeout, helpers.DefaultPollingInterval).Should(Succeed())
	})
})
