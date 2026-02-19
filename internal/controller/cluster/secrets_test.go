package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestReconcileSecrets_MultipleRefs(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}
	cluster.Spec.ACLConfigSecret = &redisv1.LocalObjectReference{Name: "acl-secret"}

	secrets := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "auth-secret",
				Namespace:       "default",
				ResourceVersion: "100",
			},
			Data: map[string][]byte{"password": []byte("pass123")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "tls-secret",
				Namespace:       "default",
				ResourceVersion: "200",
			},
			Data: map[string][]byte{"tls.crt": []byte("cert"), "tls.key": []byte("key")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "acl-secret",
				Namespace:       "default",
				ResourceVersion: "300",
			},
			Data: map[string][]byte{"acl": []byte("user default on ~* +@all")},
		},
	}

	r, c := newReconciler(append([]client.Object{cluster}, secrets...)...)
	ctx := context.Background()

	err := r.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)

	// Verify status has resource versions.
	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	assert.NotEmpty(t, updated.Status.SecretsResourceVersion)
	assert.Contains(t, updated.Status.SecretsResourceVersion, "auth-secret")
	assert.Contains(t, updated.Status.SecretsResourceVersion, "tls-secret")
	assert.Contains(t, updated.Status.SecretsResourceVersion, "acl-secret")
}

func TestReconcileSecrets_MissingSecret(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "nonexistent-tls"}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "auth-secret",
			Namespace:       "default",
			ResourceVersion: "100",
		},
		Data: map[string][]byte{"password": []byte("pass")},
	}

	r, c := newReconciler(cluster, authSecret)
	ctx := context.Background()

	// Missing TLS secret should not cause an error.
	err := r.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	// Auth secret should be tracked, TLS should not (it was missing).
	assert.Contains(t, updated.Status.SecretsResourceVersion, "auth-secret")
	_, hasTLS := updated.Status.SecretsResourceVersion["nonexistent-tls"]
	assert.False(t, hasTLS, "missing secret should not be in resource versions")
}

func TestReconcileSecrets_UpdatesResourceVersions(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "auth-secret",
			Namespace:       "default",
			ResourceVersion: "42",
		},
		Data: map[string][]byte{"password": []byte("secret")},
	}

	r, c := newReconciler(cluster, secret)
	ctx := context.Background()

	err := r.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	// The resource version should match the secret's ResourceVersion.
	rv, ok := updated.Status.SecretsResourceVersion["auth-secret"]
	assert.True(t, ok)
	assert.NotEmpty(t, rv)
}

func TestEnsureAuthSecret_GeneratesPassword(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.ensureAuthSecret(ctx, cluster, "test-auth")
	require.NoError(t, err)

	var secret corev1.Secret
	err = c.Get(ctx, types.NamespacedName{Name: "test-auth", Namespace: "default"}, &secret)
	require.NoError(t, err)

	assert.NotEmpty(t, secret.Data["password"])
	assert.Equal(t, 32, len(string(secret.Data["password"])), "password should be 32 hex chars (16 bytes)")
	assert.Equal(t, "test", secret.Labels[redisv1.LabelCluster])
	assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
}

func TestEnsureAuthSecret_ExistingNotOverwritten(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-auth",
			Namespace: "default",
		},
		Data: map[string][]byte{"password": []byte("my-password")},
	}

	r, c := newReconciler(cluster, existing)
	ctx := context.Background()

	err := r.ensureAuthSecret(ctx, cluster, "test-auth")
	require.NoError(t, err)

	var secret corev1.Secret
	err = c.Get(ctx, types.NamespacedName{Name: "test-auth", Namespace: "default"}, &secret)
	require.NoError(t, err)
	assert.Equal(t, "my-password", string(secret.Data["password"]), "existing password should not be overwritten")
}

func TestUsesSecret_EmptyMap(t *testing.T) {
	r := &ClusterReconciler{}
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			SecretsResourceVersion: nil,
		},
	}
	assert.False(t, r.UsesSecret(cluster, "any-secret"))
}

func TestUsesSecret_MatchingAndNonMatching(t *testing.T) {
	r := &ClusterReconciler{}
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			SecretsResourceVersion: map[string]string{
				"auth":   "1",
				"tls":    "2",
			},
		},
	}

	assert.True(t, r.UsesSecret(cluster, "auth"))
	assert.True(t, r.UsesSecret(cluster, "tls"))
	assert.False(t, r.UsesSecret(cluster, "unknown"))
	assert.False(t, r.UsesSecret(cluster, ""))
}
