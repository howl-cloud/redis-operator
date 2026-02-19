package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	return s
}

// generateTestCert creates a self-signed cert with the given notAfter time.
func generateTestCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"test"},
			CommonName:   "test",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return certPEM, keyPEM
}

func TestNeedsRenewal_ValidCert(t *testing.T) {
	certPEM, _ := generateTestCert(t, time.Now().Add(60*24*time.Hour)) // expires in 60 days
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
		},
	}
	assert.False(t, needsRenewal(secret))
}

func TestNeedsRenewal_ExpiresSoon(t *testing.T) {
	certPEM, _ := generateTestCert(t, time.Now().Add(15*24*time.Hour)) // expires in 15 days
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
		},
	}
	assert.True(t, needsRenewal(secret))
}

func TestNeedsRenewal_Expired(t *testing.T) {
	certPEM, _ := generateTestCert(t, time.Now().Add(-1*24*time.Hour)) // expired yesterday
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
		},
	}
	assert.True(t, needsRenewal(secret))
}

func TestNeedsRenewal_MissingKey(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{},
	}
	assert.True(t, needsRenewal(secret))
}

func TestNeedsRenewal_InvalidPEM(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": []byte("not-a-pem"),
		},
	}
	assert.True(t, needsRenewal(secret))
}

func TestNeedsRenewal_ExactlyAt30Days(t *testing.T) {
	// Certificate expiring in exactly 29 days (less than 30) should need renewal.
	certPEM, _ := generateTestCert(t, time.Now().Add(29*24*time.Hour))
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
		},
	}
	assert.True(t, needsRenewal(secret))
}

func TestParseCAFromSecret_Valid(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t, time.Now().Add(365*24*time.Hour))
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	key, cert, err := parseCAFromSecret(secret)
	require.NoError(t, err)
	assert.NotNil(t, key)
	assert.NotNil(t, cert)
	assert.Equal(t, "test", cert.Subject.CommonName)
}

func TestParseCAFromSecret_MissingKey(t *testing.T) {
	certPEM, _ := generateTestCert(t, time.Now().Add(365*24*time.Hour))
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": []byte(""), // empty
		},
	}

	_, _, err := parseCAFromSecret(secret)
	assert.Error(t, err)
}

func TestParseCAFromSecret_InvalidKeyPEM(t *testing.T) {
	certPEM, _ := generateTestCert(t, time.Now().Add(365*24*time.Hour))
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": []byte("not-a-pem"),
		},
	}

	_, _, err := parseCAFromSecret(secret)
	assert.Error(t, err)
}

func TestParseCAFromSecret_InvalidCertPEM(t *testing.T) {
	_, keyPEM := generateTestCert(t, time.Now().Add(365*24*time.Hour))
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": []byte("not-a-pem"),
			"tls.key": keyPEM,
		},
	}

	_, _, err := parseCAFromSecret(secret)
	assert.Error(t, err)
}

func TestEnsureWebhookPKI_CreatesSecrets(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	ctx := context.Background()
	err := EnsureWebhookPKI(ctx, fakeClient, "default", "redis-operator-webhook")
	require.NoError(t, err)

	// Verify CA secret was created.
	var caSecret corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", caSecretName), &caSecret)
	require.NoError(t, err)
	assert.Contains(t, string(caSecret.Data["tls.crt"]), "CERTIFICATE")
	assert.Contains(t, string(caSecret.Data["tls.key"]), "EC PRIVATE KEY")

	// Verify webhook cert secret was created.
	var webhookSecret corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &webhookSecret)
	require.NoError(t, err)
	assert.Contains(t, string(webhookSecret.Data["tls.crt"]), "CERTIFICATE")
	assert.Contains(t, string(webhookSecret.Data["tls.key"]), "EC PRIVATE KEY")
	assert.Contains(t, string(webhookSecret.Data["ca.crt"]), "CERTIFICATE")
}

func TestEnsureWebhookPKI_IdempotentOnValidCerts(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	ctx := context.Background()

	// First call creates the secrets.
	err := EnsureWebhookPKI(ctx, fakeClient, "default", "redis-operator-webhook")
	require.NoError(t, err)

	// Get the webhook cert after first call.
	var webhookSecret1 corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &webhookSecret1)
	require.NoError(t, err)
	cert1 := string(webhookSecret1.Data["tls.crt"])

	// Second call should be a no-op (certs are still valid).
	err = EnsureWebhookPKI(ctx, fakeClient, "default", "redis-operator-webhook")
	require.NoError(t, err)

	var webhookSecret2 corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &webhookSecret2)
	require.NoError(t, err)
	cert2 := string(webhookSecret2.Data["tls.crt"])

	// Certs should be unchanged.
	assert.Equal(t, cert1, cert2)
}

func TestEnsureWebhookPKI_RenewsExpiredWebhookCert(t *testing.T) {
	scheme := testScheme()

	// Create CA secret manually.
	caCertPEM, caKeyPEM := generateTestCert(t, time.Now().Add(3650*24*time.Hour))

	// Create webhook cert that expires in 5 days (needs renewal).
	webhookCertPEM, webhookKeyPEM := generateTestCert(t, time.Now().Add(5*24*time.Hour))

	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName,
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": caCertPEM,
			"tls.key": caKeyPEM,
			"ca.crt":  caCertPEM,
		},
	}

	oldWebhookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookCertSecret,
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": webhookCertPEM,
			"tls.key": webhookKeyPEM,
			"ca.crt":  caCertPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(caSecret, oldWebhookSecret).
		Build()

	ctx := context.Background()
	err := EnsureWebhookPKI(ctx, fakeClient, "default", "redis-operator-webhook")
	require.NoError(t, err)

	// Verify webhook cert was renewed (different from original).
	var newWebhookSecret corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &newWebhookSecret)
	require.NoError(t, err)
	assert.NotEqual(t, string(webhookCertPEM), string(newWebhookSecret.Data["tls.crt"]))
}

// --- Direct tests for ensureCA ---

func TestEnsureCA_CreatesNewCA(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	key, cert, err := ensureCA(ctx, fakeClient, "default")
	require.NoError(t, err)
	assert.NotNil(t, key)
	assert.NotNil(t, cert)
	assert.True(t, cert.IsCA)
	assert.Equal(t, "redis-operator-ca", cert.Subject.CommonName)

	// Verify the secret was actually created.
	var secret corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", caSecretName), &secret)
	require.NoError(t, err)
	assert.Equal(t, corev1.SecretTypeTLS, secret.Type)
	assert.Contains(t, string(secret.Data["tls.crt"]), "CERTIFICATE")
	assert.Contains(t, string(secret.Data["tls.key"]), "EC PRIVATE KEY")
	assert.Contains(t, string(secret.Data["ca.crt"]), "CERTIFICATE")
}

func TestEnsureCA_LoadsExistingCA(t *testing.T) {
	scheme := testScheme()
	caCertPEM, caKeyPEM := generateTestCert(t, time.Now().Add(3650*24*time.Hour))

	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName,
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": caCertPEM,
			"tls.key": caKeyPEM,
			"ca.crt":  caCertPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingSecret).
		Build()
	ctx := context.Background()

	key, cert, err := ensureCA(ctx, fakeClient, "default")
	require.NoError(t, err)
	assert.NotNil(t, key)
	assert.NotNil(t, cert)
	// Should load the existing cert, which has CN "test" from generateTestCert.
	assert.Equal(t, "test", cert.Subject.CommonName)
}

// --- Direct tests for ensureWebhookCert ---

func TestEnsureWebhookCert_CreatesNew(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	// First create a CA to sign the webhook cert.
	caKey, caCert, err := ensureCA(ctx, fakeClient, "default")
	require.NoError(t, err)

	err = ensureWebhookCert(ctx, fakeClient, "default", "redis-operator-webhook", caKey, caCert)
	require.NoError(t, err)

	// Verify the webhook cert secret was created.
	var secret corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &secret)
	require.NoError(t, err)
	assert.Equal(t, corev1.SecretTypeTLS, secret.Type)

	// Parse the webhook cert and verify DNS names.
	block, _ := pem.Decode(secret.Data["tls.crt"])
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Contains(t, cert.DNSNames, "redis-operator-webhook")
	assert.Contains(t, cert.DNSNames, "redis-operator-webhook.default.svc")
	assert.Contains(t, cert.DNSNames, "redis-operator-webhook.default.svc.cluster.local")
}

func TestEnsureWebhookCert_SkipsValidCert(t *testing.T) {
	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	// Create CA and webhook cert via EnsureWebhookPKI.
	err := EnsureWebhookPKI(ctx, fakeClient, "default", "redis-operator-webhook")
	require.NoError(t, err)

	// Get the cert that was created.
	var secret1 corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &secret1)
	require.NoError(t, err)
	cert1 := string(secret1.Data["tls.crt"])

	// Load the CA for the second call.
	var caSecret corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", caSecretName), &caSecret)
	require.NoError(t, err)
	caKey, caCert, err := parseCAFromSecret(&caSecret)
	require.NoError(t, err)

	// Call ensureWebhookCert again -- should skip because cert is valid.
	err = ensureWebhookCert(ctx, fakeClient, "default", "redis-operator-webhook", caKey, caCert)
	require.NoError(t, err)

	var secret2 corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &secret2)
	require.NoError(t, err)
	cert2 := string(secret2.Data["tls.crt"])

	assert.Equal(t, cert1, cert2, "valid cert should not be replaced")
}

func TestEnsureWebhookCert_RenewsExpiring(t *testing.T) {
	scheme := testScheme()

	// Create CA.
	caCertPEM, caKeyPEM := generateTestCert(t, time.Now().Add(3650*24*time.Hour))

	caSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName,
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": caCertPEM,
			"tls.key": caKeyPEM,
			"ca.crt":  caCertPEM,
		},
	}

	// Create webhook cert expiring in 5 days (needs renewal).
	webhookCertPEM, webhookKeyPEM := generateTestCert(t, time.Now().Add(5*24*time.Hour))
	webhookSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookCertSecret,
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": webhookCertPEM,
			"tls.key": webhookKeyPEM,
			"ca.crt":  caCertPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(caSecret, webhookSecret).
		Build()
	ctx := context.Background()

	caKey, caCert, err := parseCAFromSecret(caSecret)
	require.NoError(t, err)

	err = ensureWebhookCert(ctx, fakeClient, "default", "redis-operator-webhook", caKey, caCert)
	require.NoError(t, err)

	// Verify it was renewed.
	var updated corev1.Secret
	err = fakeClient.Get(ctx, clientKey("default", webhookCertSecret), &updated)
	require.NoError(t, err)
	assert.NotEqual(t, string(webhookCertPEM), string(updated.Data["tls.crt"]))
}

// client_key is a helper to create a types.NamespacedName.
func clientKey(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}
