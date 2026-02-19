package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// reconcileSecrets resolves all secret references and updates status.secretsResourceVersion.
func (r *ClusterReconciler) reconcileSecrets(ctx context.Context, cluster *redisv1.RedisCluster) error {
	logger := log.FromContext(ctx)

	// Auto-generate auth secret if not provided.
	if cluster.Spec.AuthSecret == nil {
		secretName := fmt.Sprintf("%s-auth", cluster.Name)
		if err := r.ensureAuthSecret(ctx, cluster, secretName); err != nil {
			return fmt.Errorf("ensuring auth secret: %w", err)
		}
		cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: secretName}
	}

	secretRefs := map[string]*redisv1.LocalObjectReference{
		"authSecret":              cluster.Spec.AuthSecret,
		"aclConfigSecret":         cluster.Spec.ACLConfigSecret,
		"tlsSecret":               cluster.Spec.TLSSecret,
		"caSecret":                cluster.Spec.CASecret,
		"backupCredentialsSecret": cluster.Spec.BackupCredentialsSecret,
	}

	newVersions := make(map[string]string)
	for refName, ref := range secretRefs {
		if ref == nil {
			continue
		}
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ref.Name,
			Namespace: cluster.Namespace,
		}, &secret); err != nil {
			if errors.IsNotFound(err) {
				logger.Info("Secret not found", "secret", ref.Name, "ref", refName)
				continue
			}
			return fmt.Errorf("getting secret %s: %w", ref.Name, err)
		}
		newVersions[ref.Name] = secret.ResourceVersion
	}

	// Check if any secret versions changed (rotation).
	for name, newVer := range newVersions {
		if oldVer, ok := cluster.Status.SecretsResourceVersion[name]; ok && oldVer != newVer {
			r.Recorder.Eventf(cluster, corev1.EventTypeNormal, "SecretRotated", "Secret %s rotated (resourceVersion %s -> %s)", name, oldVer, newVer)
		}
	}

	patch := client.MergeFrom(cluster.DeepCopy())
	cluster.Status.SecretsResourceVersion = newVersions
	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		return fmt.Errorf("patching secrets resource version: %w", err)
	}

	return nil
}

// ensureAuthSecret creates a random auth secret if it does not exist.
func (r *ClusterReconciler) ensureAuthSecret(ctx context.Context, cluster *redisv1.RedisCluster, secretName string) error {
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: cluster.Namespace,
	}, &existing)
	if err == nil {
		return nil // Already exists.
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking auth secret: %w", err)
	}

	passwordBytes := make([]byte, 16)
	if _, err := rand.Read(passwordBytes); err != nil {
		return fmt.Errorf("generating random password: %w", err)
	}
	password := hex.EncodeToString(passwordBytes)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster: cluster.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte(password),
		},
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating auth secret: %w", err)
	}

	return nil
}

// UsesSecret checks if the cluster references the given secret name.
func (r *ClusterReconciler) UsesSecret(cluster *redisv1.RedisCluster, secretName string) bool {
	_, ok := cluster.Status.SecretsResourceVersion[secretName]
	return ok
}
