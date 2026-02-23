package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	managercontroller "github.com/howl-cloud/redis-operator/internal/cmd/manager/controller"
	"github.com/howl-cloud/redis-operator/webhooks"
)

const (
	pkiWebhookServiceName      = "localhost"
	pkiRotationTimeout         = 30 * time.Second
	pkiRotationPollingInterval = 250 * time.Millisecond
	pkiReconcileInterval       = 250 * time.Millisecond
	pkiWebhookCertSecretName   = "redis-operator-webhook-cert"
	pkiCASecretName            = "redis-operator-ca-secret"
	admissionTrafficInterval   = 120 * time.Millisecond
)

var _ = Describe("Webhook PKI rotation", func() {
	It("rotates webhook certs, updates caBundle, and keeps admission requests serving continuously", func() {
		ctx := context.Background()
		namespace := uniqueName("pki")
		podName := uniqueName("operator")
		mutatingWebhookName := uniqueName("redis-operator-mutating")
		validatingWebhookName := uniqueName("redis-operator-validating")

		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())

		operatorPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "manager",
						Image: "redis-operator:latest",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, operatorPod)).To(Succeed())

		caCertPEM, caKeyPEM, caKey, caCert, err := generateTestCA(time.Now().Add(3650 * 24 * time.Hour))
		Expect(err).NotTo(HaveOccurred())

		initialWebhookCertPEM, initialWebhookKeyPEM, err := generateSignedWebhookCert(
			namespace,
			pkiWebhookServiceName,
			caKey,
			caCert,
			time.Now().Add(365*24*time.Hour),
		)
		Expect(err).NotTo(HaveOccurred())

		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pkiCASecretName,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": caCertPEM,
				"tls.key": caKeyPEM,
				"ca.crt":  caCertPEM,
			},
		}
		Expect(k8sClient.Create(ctx, caSecret)).To(Succeed())

		webhookSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pkiWebhookCertSecretName,
				Namespace: namespace,
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{
				"tls.crt": initialWebhookCertPEM,
				"tls.key": initialWebhookKeyPEM,
				"ca.crt":  caCertPEM,
			},
		}
		Expect(k8sClient.Create(ctx, webhookSecret)).To(Succeed())

		webhookPort, err := reserveLocalPort()
		Expect(err).NotTo(HaveOccurred())

		certDir, err := os.MkdirTemp("", "redis-operator-webhook-certs-*")
		Expect(err).NotTo(HaveOccurred())
		Expect(writeWebhookCertFiles(certDir, initialWebhookCertPEM, initialWebhookKeyPEM)).To(Succeed())

		mutatingCfg := &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: mutatingWebhookName},
			Webhooks: []admissionregistrationv1.MutatingWebhook{
				{
					Name: "mrediscluster.redis.io",
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						CABundle: []byte{},
						URL:      stringPtr(fmt.Sprintf("https://localhost:%d/mutate-redis-io-v1-rediscluster", webhookPort)),
					},
					AdmissionReviewVersions: []string{"v1"},
					Rules: []admissionregistrationv1.RuleWithOperations{
						{
							Operations: []admissionregistrationv1.OperationType{
								admissionregistrationv1.Create,
								admissionregistrationv1.Update,
							},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{"redis.io"},
								APIVersions: []string{"v1"},
								Resources:   []string{"redisclusters"},
								Scope:       scopeTypePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
					SideEffects:   sideEffectClassPtr(admissionregistrationv1.SideEffectClassNone),
					FailurePolicy: failurePolicyPtr(admissionregistrationv1.Fail),
				},
			},
		}
		Expect(k8sClient.Create(ctx, mutatingCfg)).To(Succeed())

		validatingCfg := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: validatingWebhookName},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{
				{
					Name: "vrediscluster.redis.io",
					ClientConfig: admissionregistrationv1.WebhookClientConfig{
						CABundle: []byte{},
						URL:      stringPtr(fmt.Sprintf("https://localhost:%d/validate-redis-io-v1-rediscluster", webhookPort)),
					},
					AdmissionReviewVersions: []string{"v1"},
					Rules: []admissionregistrationv1.RuleWithOperations{
						{
							Operations: []admissionregistrationv1.OperationType{
								admissionregistrationv1.Create,
								admissionregistrationv1.Update,
							},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{"redis.io"},
								APIVersions: []string{"v1"},
								Resources:   []string{"redisclusters"},
								Scope:       scopeTypePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
					SideEffects:   sideEffectClassPtr(admissionregistrationv1.SideEffectClassNone),
					FailurePolicy: failurePolicyPtr(admissionregistrationv1.Fail),
				},
			},
		}
		Expect(k8sClient.Create(ctx, validatingCfg)).To(Succeed())

		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, operatorPod)
			_ = k8sClient.Delete(ctx, validatingCfg)
			_ = k8sClient.Delete(ctx, mutatingCfg)
			_ = k8sClient.Delete(ctx, webhookSecret)
			_ = k8sClient.Delete(ctx, caSecret)
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
			_ = os.RemoveAll(certDir)
		})

		scheme := runtime.NewScheme()
		utilruntime.Must(clientgoscheme.AddToScheme(scheme))
		utilruntime.Must(admissionregistrationv1.AddToScheme(scheme))
		utilruntime.Must(redisv1.AddToScheme(scheme))

		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme: scheme,
			Metrics: metricsserver.Options{
				BindAddress: "0",
			},
			HealthProbeBindAddress: "0",
			LeaderElection:         false,
			WebhookServer: webhook.NewServer(webhook.Options{
				Host:    "localhost",
				Port:    webhookPort,
				CertDir: certDir,
			}),
		})
		Expect(err).NotTo(HaveOccurred())

		defaulter := &webhooks.RedisClusterDefaulter{}
		Expect(defaulter.SetupWebhookWithManager(mgr)).To(Succeed())
		validator := &webhooks.RedisClusterValidator{}
		Expect(validator.SetupValidatingWebhookWithManager(mgr)).To(Succeed())

		pkiClient, err := client.New(cfg, client.Options{Scheme: scheme})
		Expect(err).NotTo(HaveOccurred())

		pkiOptions := managercontroller.WebhookPKIOptions{
			Namespace:                          namespace,
			ServiceName:                        pkiWebhookServiceName,
			PodName:                            podName,
			MutatingWebhookConfigurationName:   mutatingWebhookName,
			ValidatingWebhookConfigurationName: validatingWebhookName,
			EventRecorder:                      mgr.GetEventRecorderFor("redis-operator-e2e-pki"),
		}

		Expect(mgr.Add(managercontroller.NewWebhookPKIReconciler(pkiClient, pkiOptions, pkiReconcileInterval))).To(Succeed())

		syncCtx, stopSync := context.WithCancel(ctx)
		go syncWebhookCertFilesFromSecret(syncCtx, k8sClient, namespace, certDir)
		DeferCleanup(stopSync)

		mgrCtx, stopMgr := context.WithCancel(ctx)
		DeferCleanup(stopMgr)
		go func() {
			defer GinkgoRecover()
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace,
				Name:      pkiCASecretName,
			}, caSecret)).To(Succeed())

			var gotMutating admissionregistrationv1.MutatingWebhookConfiguration
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mutatingWebhookName}, &gotMutating)).To(Succeed())
			g.Expect(gotMutating.Webhooks).NotTo(BeEmpty())
			g.Expect(gotMutating.Webhooks[0].ClientConfig.CABundle).NotTo(BeEmpty())

			var gotValidating admissionregistrationv1.ValidatingWebhookConfiguration
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: validatingWebhookName}, &gotValidating)).To(Succeed())
			g.Expect(gotValidating.Webhooks).NotTo(BeEmpty())
			g.Expect(gotValidating.Webhooks[0].ClientConfig.CABundle).NotTo(BeEmpty())
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())

		// Prove admission webhooks are actually serving requests by asserting defaults are applied.
		defaultProbeName := uniqueName("admission-probe")
		defaultProbe := &redisv1.RedisCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      defaultProbeName,
				Namespace: namespace,
			},
			Spec: redisv1.RedisClusterSpec{
				Storage: redisv1.StorageSpec{
					Size: resource.MustParse("1Gi"),
				},
			},
		}
		Expect(k8sClient.Create(ctx, defaultProbe)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, defaultProbe)
		})
		Eventually(func(g Gomega) {
			var fresh redisv1.RedisCluster
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: defaultProbeName, Namespace: namespace}, &fresh)).To(Succeed())
			g.Expect(fresh.Spec.Instances).To(Equal(int32(1)))
			g.Expect(fresh.Spec.ImageName).To(Equal("redis:7.2"))
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())

		var admissionFailures atomic.Int64
		admissionCtx, stopAdmission := context.WithCancel(ctx)
		go generateAdmissionTraffic(admissionCtx, k8sClient, namespace, &admissionFailures)
		DeferCleanup(stopAdmission)

		// Wait for steady-state successful admissions before triggering rotation.
		Eventually(func(g Gomega) {
			g.Expect(admissionFailures.Load()).To(BeZero())
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())

		// Force renewal by replacing the current webhook cert with a near-expiry cert.
		shortCertPEM, shortKeyPEM, err := generateSignedWebhookCert(
			namespace,
			pkiWebhookServiceName,
			caKey,
			caCert,
			time.Now().Add(1*time.Hour),
		)
		Expect(err).NotTo(HaveOccurred())
		Expect(verifyWebhookCert(shortCertPEM, caSecret.Data["ca.crt"], pkiWebhookServiceName)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace,
				Name:      pkiWebhookCertSecretName,
			}, webhookSecret)).To(Succeed())
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())

		webhookSecret.Data["tls.crt"] = shortCertPEM
		webhookSecret.Data["tls.key"] = shortKeyPEM
		Expect(k8sClient.Update(ctx, webhookSecret)).To(Succeed())

		Eventually(func(g Gomega) {
			var rotatedSecret corev1.Secret
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: namespace,
				Name:      pkiWebhookCertSecretName,
			}, &rotatedSecret)).To(Succeed())

			g.Expect(rotatedSecret.Data["tls.crt"]).NotTo(Equal(shortCertPEM))
			g.Expect(verifyWebhookCert(rotatedSecret.Data["tls.crt"], caSecret.Data["ca.crt"], pkiWebhookServiceName)).To(Succeed())
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())

		// During and after rotation, admission requests should continue succeeding.
		Consistently(func() int64 {
			return admissionFailures.Load()
		}, 4*time.Second, pkiRotationPollingInterval).Should(BeZero())

		Eventually(func(g Gomega) {
			var gotMutating admissionregistrationv1.MutatingWebhookConfiguration
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mutatingWebhookName}, &gotMutating)).To(Succeed())
			g.Expect(gotMutating.Webhooks).NotTo(BeEmpty())
			g.Expect(gotMutating.Webhooks[0].ClientConfig.CABundle).To(Equal(caSecret.Data["ca.crt"]))

			var gotValidating admissionregistrationv1.ValidatingWebhookConfiguration
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: validatingWebhookName}, &gotValidating)).To(Succeed())
			g.Expect(gotValidating.Webhooks).NotTo(BeEmpty())
			g.Expect(gotValidating.Webhooks[0].ClientConfig.CABundle).To(Equal(caSecret.Data["ca.crt"]))
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(ctx, &events, client.InNamespace(namespace))).To(Succeed())

			found := false
			for i := range events.Items {
				event := events.Items[i]
				if event.Reason != "CertRotated" {
					continue
				}
				if event.InvolvedObject.Kind != "Pod" || event.InvolvedObject.Name != podName {
					continue
				}
				found = true
				break
			}
			g.Expect(found).To(BeTrue(), "expected CertRotated event for operator pod")
		}, pkiRotationTimeout, pkiRotationPollingInterval).Should(Succeed())
	})
})

func verifyWebhookCert(certPEM, caPEM []byte, dnsName string) error {
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return err
	}
	caCert, err := parseCertificate(caPEM)
	if err != nil {
		return err
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	_, err = cert.Verify(x509.VerifyOptions{
		DNSName: dnsName,
		Roots:   roots,
	})
	return err
}

func parseCertificate(certPEM []byte) (*x509.Certificate, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, x509.ErrUnsupportedAlgorithm
	}
	return x509.ParseCertificate(certBlock.Bytes)
}

func generateSignedWebhookCert(
	namespace, serviceName string,
	caKey *ecdsa.PrivateKey,
	caCert *x509.Certificate,
	notAfter time.Time,
) ([]byte, []byte, error) {
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	dnsNames := []string{
		serviceName,
		serviceName + "." + namespace,
		serviceName + "." + namespace + ".svc",
		serviceName + "." + namespace + ".svc.cluster.local",
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			Organization: []string{"redis-operator"},
			CommonName:   serviceName,
		},
		DNSNames:  dnsNames,
		NotBefore: time.Now().Add(-1 * time.Minute),
		NotAfter:  notAfter,
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func generateTestCA(notAfter time.Time) ([]byte, []byte, *ecdsa.PrivateKey, *x509.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"redis-operator"},
			CommonName:   "redis-operator-ca",
		},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, caKey, cert, nil
}

func reserveLocalPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func writeWebhookCertFiles(certDir string, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomically(filepath.Join(certDir, "tls.crt"), certPEM, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomically(filepath.Join(certDir, "tls.key"), keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func writeFileAtomically(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func syncWebhookCertFilesFromSecret(ctx context.Context, c client.Client, namespace, certDir string) {
	var lastCert []byte
	var lastKey []byte
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var secret corev1.Secret
			if err := c.Get(ctx, types.NamespacedName{
				Namespace: namespace,
				Name:      pkiWebhookCertSecretName,
			}, &secret); err != nil {
				continue
			}

			cert := secret.Data["tls.crt"]
			key := secret.Data["tls.key"]
			if len(cert) == 0 || len(key) == 0 {
				continue
			}

			if string(cert) == string(lastCert) && string(key) == string(lastKey) {
				continue
			}

			if err := writeWebhookCertFiles(certDir, cert, key); err != nil {
				continue
			}
			lastCert = append([]byte(nil), cert...)
			lastKey = append([]byte(nil), key...)
		}
	}
}

func generateAdmissionTraffic(ctx context.Context, c client.Client, namespace string, failures *atomic.Int64) {
	ticker := time.NewTicker(admissionTrafficInterval)
	defer ticker.Stop()
	var counter int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			counter++
			name := fmt.Sprintf("admit-%d-%d", time.Now().UnixNano(), counter)
			cluster := &redisv1.RedisCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Spec: redisv1.RedisClusterSpec{
					Storage: redisv1.StorageSpec{
						Size: resource.MustParse("1Gi"),
					},
				},
			}
			if err := c.Create(ctx, cluster); err != nil {
				failures.Add(1)
				continue
			}
			_ = c.Delete(ctx, cluster)
		}
	}
}

func stringPtr(v string) *string {
	return &v
}

func sideEffectClassPtr(v admissionregistrationv1.SideEffectClass) *admissionregistrationv1.SideEffectClass {
	return &v
}

func failurePolicyPtr(v admissionregistrationv1.FailurePolicyType) *admissionregistrationv1.FailurePolicyType {
	return &v
}

func scopeTypePtr(v admissionregistrationv1.ScopeType) *admissionregistrationv1.ScopeType {
	return &v
}
