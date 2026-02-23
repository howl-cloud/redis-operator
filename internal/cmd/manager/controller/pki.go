// Package controller contains the PKI management for webhook TLS.
package controller

import (
	"bytes"
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

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	caSecretName      = "redis-operator-ca-secret"
	webhookCertSecret = "redis-operator-webhook-cert"
	certValidityDays  = 365
	caValidityDays    = 3650 // ~10 years
	renewalThreshold  = 30 * 24 * time.Hour
)

// WebhookPKIOptions configures webhook PKI reconciliation.
type WebhookPKIOptions struct {
	Namespace                          string
	ServiceName                        string
	PodName                            string
	MutatingWebhookConfigurationName   string
	ValidatingWebhookConfigurationName string
	EventRecorder                      record.EventRecorder
}

// EnsureWebhookPKI ensures the CA and webhook certificate secrets exist and are valid.
func EnsureWebhookPKI(ctx context.Context, c client.Client, options WebhookPKIOptions) error {
	logger := log.FromContext(ctx)

	// Step 1: Ensure CA secret.
	caKey, caCert, err := ensureCA(ctx, c, options.Namespace)
	if err != nil {
		return fmt.Errorf("ensuring CA: %w", err)
	}
	logger.Info("CA secret ensured", "namespace", options.Namespace)

	// Step 2: Ensure webhook certificate signed by CA.
	rotated, notAfter, err := ensureWebhookCert(ctx, c, options.Namespace, options.ServiceName, caKey, caCert)
	if err != nil {
		return fmt.Errorf("ensuring webhook cert: %w", err)
	}
	logger.Info("Webhook certificate ensured", "namespace", options.Namespace)

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	if err := patchWebhookConfigurations(
		ctx,
		c,
		caCertPEM,
		options.MutatingWebhookConfigurationName,
		options.ValidatingWebhookConfigurationName,
	); err != nil {
		return fmt.Errorf("patching webhook configurations: %w", err)
	}
	logger.Info(
		"Webhook configurations ensured",
		"mutating", options.MutatingWebhookConfigurationName,
		"validating", options.ValidatingWebhookConfigurationName,
	)

	if rotated {
		logger.Info("Webhook certificate rotated", "namespace", options.Namespace, "new-expiry", notAfter.Format(time.RFC3339))
		emitRotationEvent(ctx, c, options, notAfter)
	}

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
func ensureWebhookCert(
	ctx context.Context,
	c client.Client,
	namespace, serviceName string,
	caKey *ecdsa.PrivateKey,
	caCert *x509.Certificate,
) (bool, time.Time, error) {
	var secret corev1.Secret
	createNew := false
	rotated := false
	getErr := c.Get(ctx, types.NamespacedName{
		Name: webhookCertSecret, Namespace: namespace,
	}, &secret)
	if getErr == nil {
		// Check if cert is still valid (renew if within 30 days of expiry).
		if !needsRenewal(&secret) {
			return false, time.Time{}, nil
		}
		rotated = true
	} else if !errors.IsNotFound(getErr) {
		return false, time.Time{}, fmt.Errorf("getting webhook cert secret: %w", getErr)
	} else {
		createNew = true
	}

	// Generate webhook server key and certificate.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("generating server key: %w", err)
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
		return false, time.Time{}, fmt.Errorf("creating server certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("marshaling server key: %w", err)
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
		if err := c.Create(ctx, &newSecret); err != nil {
			return false, time.Time{}, err
		}
		return false, serverTemplate.NotAfter, nil
	}
	// Update existing.
	secret.Data = newSecret.Data
	if err := c.Update(ctx, &secret); err != nil {
		return false, time.Time{}, err
	}
	return rotated, serverTemplate.NotAfter, nil
}

func patchWebhookConfigurations(
	ctx context.Context,
	c client.Client,
	caCertPEM []byte,
	mutatingName, validatingName string,
) error {
	if mutatingName != "" {
		if err := patchMutatingWebhookConfiguration(ctx, c, mutatingName, caCertPEM); err != nil {
			return err
		}
	}
	if validatingName != "" {
		if err := patchValidatingWebhookConfiguration(ctx, c, validatingName, caCertPEM); err != nil {
			return err
		}
	}
	return nil
}

func patchMutatingWebhookConfiguration(ctx context.Context, c client.Client, name string, caCertPEM []byte) error {
	var cfg admissionregistrationv1.MutatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &cfg); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting mutating webhook configuration %q: %w", name, err)
	}
	updated := false
	for i := range cfg.Webhooks {
		if bytes.Equal(cfg.Webhooks[i].ClientConfig.CABundle, caCertPEM) {
			continue
		}
		cfg.Webhooks[i].ClientConfig.CABundle = caCertPEM
		updated = true
	}
	if !updated {
		return nil
	}
	if err := c.Update(ctx, &cfg); err != nil {
		return fmt.Errorf("updating mutating webhook configuration %q: %w", name, err)
	}
	return nil
}

func patchValidatingWebhookConfiguration(ctx context.Context, c client.Client, name string, caCertPEM []byte) error {
	var cfg admissionregistrationv1.ValidatingWebhookConfiguration
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &cfg); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting validating webhook configuration %q: %w", name, err)
	}
	updated := false
	for i := range cfg.Webhooks {
		if bytes.Equal(cfg.Webhooks[i].ClientConfig.CABundle, caCertPEM) {
			continue
		}
		cfg.Webhooks[i].ClientConfig.CABundle = caCertPEM
		updated = true
	}
	if !updated {
		return nil
	}
	if err := c.Update(ctx, &cfg); err != nil {
		return fmt.Errorf("updating validating webhook configuration %q: %w", name, err)
	}
	return nil
}

func emitRotationEvent(ctx context.Context, c client.Client, options WebhookPKIOptions, notAfter time.Time) {
	if options.EventRecorder == nil {
		return
	}
	logger := log.FromContext(ctx)
	if options.PodName == "" {
		logger.Info("Skipped PKI rotation event; pod name is empty")
		return
	}

	var pod corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: options.Namespace, Name: options.PodName}, &pod); err != nil {
		logger.Error(err, "Failed to emit PKI rotation event; operator pod lookup failed", "pod", options.PodName, "namespace", options.Namespace)
		return
	}

	options.EventRecorder.Eventf(
		&pod,
		corev1.EventTypeNormal,
		"CertRotated",
		"Webhook TLS certificate rotated; new expiry: %s",
		notAfter.UTC().Format(time.RFC3339),
	)
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
	return time.Until(cert.NotAfter) < renewalThreshold
}
