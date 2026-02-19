// Package controller contains the PKI management for webhook TLS.
package controller

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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	caSecretName      = "redis-operator-ca-secret"
	webhookCertSecret = "redis-operator-webhook-cert"
	certValidityDays  = 365
	caValidityDays    = 3650 // ~10 years
)

// EnsureWebhookPKI ensures the CA and webhook certificate secrets exist and are valid.
func EnsureWebhookPKI(ctx context.Context, c client.Client, namespace, serviceName string) error {
	logger := log.FromContext(ctx)

	// Step 1: Ensure CA secret.
	caKey, caCert, err := ensureCA(ctx, c, namespace)
	if err != nil {
		return fmt.Errorf("ensuring CA: %w", err)
	}
	logger.Info("CA secret ensured", "namespace", namespace)

	// Step 2: Ensure webhook certificate signed by CA.
	if err := ensureWebhookCert(ctx, c, namespace, serviceName, caKey, caCert); err != nil {
		return fmt.Errorf("ensuring webhook cert: %w", err)
	}
	logger.Info("Webhook certificate ensured", "namespace", namespace)

	return nil
}

// ensureCA creates or loads the CA keypair from the Secret.
func ensureCA(ctx context.Context, c client.Client, namespace string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{
		Name: caSecretName, Namespace: namespace,
	}, &secret)

	if err == nil {
		// Parse existing CA.
		return parseCAFromSecret(&secret)
	}
	if !errors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("getting CA secret: %w", err)
	}

	// Generate new CA.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA key: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"redis-operator"},
			CommonName:   "redis-operator-ca",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(caValidityDays * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA certificate: %w", err)
	}

	// Encode PEM.
	keyDER, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshaling CA key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})

	// Store in Secret.
	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  certPEM,
		},
	}

	if err := c.Create(ctx, &secret); err != nil {
		return nil, nil, fmt.Errorf("creating CA secret: %w", err)
	}

	return caKey, caCert, nil
}

// ensureWebhookCert creates or refreshes the webhook TLS certificate.
func ensureWebhookCert(ctx context.Context, c client.Client, namespace, serviceName string, caKey *ecdsa.PrivateKey, caCert *x509.Certificate) error {
	var secret corev1.Secret
	createNew := false
	getErr := c.Get(ctx, types.NamespacedName{
		Name: webhookCertSecret, Namespace: namespace,
	}, &secret)
	if getErr == nil {
		// Check if cert is still valid (renew if within 30 days of expiry).
		if !needsRenewal(&secret) {
			return nil
		}
	} else if !errors.IsNotFound(getErr) {
		return fmt.Errorf("getting webhook cert secret: %w", getErr)
	} else {
		createNew = true
	}

	// Generate webhook server key and certificate.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating server key: %w", err)
	}

	dnsNames := []string{
		serviceName,
		fmt.Sprintf("%s.%s", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"redis-operator"},
			CommonName:   serviceName,
		},
		DNSNames:  dnsNames,
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(certValidityDays * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("creating server certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return fmt.Errorf("marshaling server key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})

	newSecret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookCertSecret,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
			"ca.crt":  caCertPEM,
		},
	}

	if createNew {
		return c.Create(ctx, &newSecret)
	}
	// Update existing.
	secret.Data = newSecret.Data
	return c.Update(ctx, &secret)
}

// parseCAFromSecret loads the CA key and cert from a Secret.
func parseCAFromSecret(secret *corev1.Secret) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	keyBlock, _ := pem.Decode(secret.Data["tls.key"])
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("no PEM block found in CA key")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA key: %w", err)
	}

	certBlock, _ := pem.Decode(secret.Data["tls.crt"])
	if certBlock == nil {
		return nil, nil, fmt.Errorf("no PEM block found in CA cert")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA cert: %w", err)
	}

	return key, cert, nil
}

// needsRenewal checks if the webhook cert in the secret will expire within 30 days.
func needsRenewal(secret *corev1.Secret) bool {
	certPEM, ok := secret.Data["tls.crt"]
	if !ok {
		return true
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true
	}
	return time.Until(cert.NotAfter) < 30*24*time.Hour
}
